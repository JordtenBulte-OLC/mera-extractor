// internal/api/inflight_test.go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"mera-extractor/internal/gitops"
	"mera-extractor/internal/mx"
)

// blockClone makes CloneBoth wait on release, counting calls and recording
// the context it ran with, so a test can line requests up while a run is
// still in flight.
func (h *harness) blockClone() (release chan struct{}, calls *int, runCtx *context.Context) {
	release = make(chan struct{})
	calls = new(int)
	runCtx = new(context.Context)
	inner := h.srv.Deps.CloneBoth
	h.srv.Deps.CloneBoth = func(ctx context.Context, workRoot string, req gitops.CloneBothRequest) (gitops.CloneBothResult, error) {
		h.mu.Lock()
		*calls++
		*runCtx = ctx
		h.mu.Unlock()
		select {
		case <-release:
		case <-ctx.Done():
			return gitops.CloneBothResult{}, ctx.Err()
		}
		return inner(ctx, workRoot, req)
	}
	return release, calls, runCtx
}

// waitForWaiters blocks until n callers are waiting on some in-flight run.
func waitForWaiters(t *testing.T, s *Server, n int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		s.inflight.mu.Lock()
		total := 0
		for _, fl := range s.inflight.m {
			total += fl.waiters
		}
		s.inflight.mu.Unlock()
		if total == n {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %d waiters", n)
}

type postResult struct {
	code int
	body string
}

func (h *harness) postAsync(ctx context.Context, body string) <-chan postResult {
	ch := make(chan postResult, 1)
	go func() {
		r := httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(body)).WithContext(ctx)
		w := httptest.NewRecorder()
		h.srv.handleExtract(w, r)
		ch <- postResult{w.Code, w.Body.String()}
	}()
	return ch
}

const reqA = `{"requestId":"A","repoUrl":"https://git.example/x.git","pat":"p1","baseSha":"aaaaaaa","headSha":"bbbbbbb"}`
const reqB = `{"requestId":"B","repoUrl":"https://git.example/x.git","pat":"p1","baseSha":"aaaaaaa","headSha":"bbbbbbb"}`

func TestExtract_IdenticalConcurrentRequestsShareOneRun(t *testing.T) {
	h := newHarness(t)
	release, calls, _ := h.blockClone()

	a := h.postAsync(context.Background(), reqA)
	waitForWaiters(t, h.srv, 1)
	b := h.postAsync(context.Background(), reqB)
	waitForWaiters(t, h.srv, 2)
	close(release)

	for _, res := range []struct {
		ch   <-chan postResult
		want string
	}{{a, "A"}, {b, "B"}} {
		got := <-res.ch
		if got.code != http.StatusOK {
			t.Fatalf("status %d: %s", got.code, got.body)
		}
		var resp extractResponse
		if err := json.Unmarshal([]byte(got.body), &resp); err != nil {
			t.Fatal(err)
		}
		// requestId is per caller even though the body is shared.
		if resp.RequestID != res.want {
			t.Errorf("requestId = %q, want %q", resp.RequestID, res.want)
		}
	}
	if *calls != 1 {
		t.Errorf("CloneBoth ran %d times, want 1", *calls)
	}
	if len(h.cleanupCalls) != 1 {
		t.Errorf("cleanup ran %d times, want 1", len(h.cleanupCalls))
	}
}

// Anything that changes what is fetched, or with whose credentials, must
// get a run of its own. The PAT case is the security one: a caller must not
// receive a model its own credentials never fetched.
func TestExtract_DifferentRequestsDoNotShare(t *testing.T) {
	for name, other := range map[string]string{
		"head": strings.Replace(reqB, "bbbbbbb", "ccccccc", 1),
		"base": strings.Replace(reqB, "aaaaaaa", "ddddddd", 1),
		"pat":  strings.Replace(reqB, `"pat":"p1"`, `"pat":"p2"`, 1),
		"repo": strings.Replace(reqB, "x.git", "y.git", 1),
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			release, calls, _ := h.blockClone()
			a := h.postAsync(context.Background(), reqA)
			b := h.postAsync(context.Background(), other)
			waitForWaiters(t, h.srv, 2)
			close(release)
			<-a
			<-b
			if *calls != 2 {
				t.Errorf("CloneBoth ran %d times, want 2", *calls)
			}
		})
	}
}

// Collapsing is for overlap only; it is not a cache.
func TestExtract_SequentialIdenticalRequestsRunTwice(t *testing.T) {
	h := newHarness(t)
	h.post(reqA)
	h.post(reqA)
	if len(h.cleanupCalls) != 2 {
		t.Errorf("want two independent runs, got %d cleanups", len(h.cleanupCalls))
	}
}

// The first caller hanging up must not take the run away from a caller that
// joined it later.
func TestExtract_LeaderLeavingDoesNotCancelTheSharedRun(t *testing.T) {
	h := newHarness(t)
	release, calls, runCtx := h.blockClone()

	ctxA, cancelA := context.WithCancel(context.Background())
	a := h.postAsync(ctxA, reqA)
	waitForWaiters(t, h.srv, 1)
	b := h.postAsync(context.Background(), reqB)
	waitForWaiters(t, h.srv, 2)

	cancelA()
	if got := <-a; got.code != statusClientClosedRequest {
		t.Errorf("leader status = %d, want %d", got.code, statusClientClosedRequest)
	}
	h.mu.Lock()
	err := (*runCtx).Err()
	h.mu.Unlock()
	if err != nil {
		t.Fatalf("run context cancelled while a caller still waits: %v", err)
	}

	close(release)
	if got := <-b; got.code != http.StatusOK {
		t.Errorf("follower status = %d, want 200: %s", got.code, got.body)
	}
	if *calls != 1 {
		t.Errorf("CloneBoth ran %d times, want 1", *calls)
	}
}

// With nobody left waiting, the run is cancelled, exactly as a lone
// caller's disconnect always cancelled its own request.
func TestExtract_LastCallerLeavingCancelsTheRun(t *testing.T) {
	h := newHarness(t)
	_, _, runCtx := h.blockClone()

	ctxA, cancelA := context.WithCancel(context.Background())
	a := h.postAsync(ctxA, reqA)
	waitForWaiters(t, h.srv, 1)
	cancelA()
	<-a

	deadline := time.Now().Add(5 * time.Second)
	for {
		h.mu.Lock()
		err := (*runCtx).Err()
		h.mu.Unlock()
		if err != nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("run context never cancelled after its only caller left")
		}
		time.Sleep(5 * time.Millisecond)
	}

	// The slot is free again: a new identical request starts a fresh run
	// instead of joining the dying one.
	h.srv.inflight.mu.Lock()
	n := len(h.srv.inflight.m)
	h.srv.inflight.mu.Unlock()
	if n != 0 {
		t.Errorf("abandoned run still registered (%d entries)", n)
	}
}

// The shared run is a plain goroutine, outside net/http's per-request
// recovery. A panic in it must become a 500, not a dead server.
func TestExtract_PanicInSharedRunIsA500(t *testing.T) {
	captureLog(t)
	h := newHarness(t)
	h.srv.Deps.Diff = func(ctx context.Context, bin mx.Binary, basePath, headPath, outPath string) (mx.DiffResult, error) {
		panic("boom")
	}
	w := h.post(reqA)
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if len(h.cleanupCalls) != 1 {
		t.Errorf("cleanup must still run after a panic, got %d", len(h.cleanupCalls))
	}
}

// dump-mpr's export must land in the request's workdir, where Cleanup and
// the workspace janitor reach it, and not in os.TempDir().
func TestExtract_DumpMprWritesIntoTheWorkDir(t *testing.T) {
	h := newHarness(t)
	h.setDiff(mx.UnitDifference{Type: "Microflows$Microflow", ID: "x", Change: "Modified"})
	var mu sync.Mutex
	var dirs []string
	h.srv.Deps.ResolveQualifiedNames = func(ctx context.Context, bin mx.Binary, mprPath, outDir string, unitTypes []string, wantIDs map[string]bool) (map[string]mx.ResolvedUnit, error) {
		mu.Lock()
		dirs = append(dirs, outDir)
		mu.Unlock()
		return map[string]mx.ResolvedUnit{}, nil
	}
	decodeExtract(t, h.postDefault())
	if len(dirs) != 2 {
		t.Fatalf("want one dump-mpr call per side, got %v", dirs)
	}
	for _, d := range dirs {
		if d != h.workDir {
			t.Errorf("outDir = %q, want the clone workdir %q", d, h.workDir)
		}
	}
}

// The "joined" line must appear while the shared run is still going, not
// when it ends: that can be minutes later.
func TestExtract_JoinIsLoggedBeforeTheRunFinishes(t *testing.T) {
	captureLog(t) // restores the logger afterwards
	buf := &syncBuffer{}
	log.SetOutput(buf) // read while requests still log: needs the lock
	h := newHarness(t)
	release, _, _ := h.blockClone()

	a := h.postAsync(context.Background(), reqA)
	waitForWaiters(t, h.srv, 1)
	b := h.postAsync(context.Background(), reqB)
	waitForWaiters(t, h.srv, 2)

	// waiters hits 2 inside join, just before the log line; give it a moment.
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(buf.String(), "extract joined in-flight run") {
		if time.Now().After(deadline) {
			t.Fatalf("no joined line while the run is still blocked:\n%s", buf.String())
		}
		time.Sleep(5 * time.Millisecond)
	}
	if !strings.Contains(buf.String(), "(running: clone:") {
		t.Errorf("joined line should say which stage the run is in:\n%s", buf.String())
	}
	close(release)
	<-a
	<-b
}

// syncBuffer is a bytes.Buffer safe to read while the logger writes to it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
