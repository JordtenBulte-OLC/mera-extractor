// internal/workspace/workspace_test.go
package workspace

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// --------------------------------------------------------------------------
// helpers
// --------------------------------------------------------------------------

// quietManager builds a Manager under a fresh root with the janitor disabled
// (tests drive reap directly) and log output captured rather than printed.
func quietManager(t *testing.T, opts ...Option) (*Manager, *logSink) {
	t.Helper()
	sink := &logSink{}
	base := []Option{WithSweepInterval(0), WithLogf(sink.Printf)}
	m, err := New(t.TempDir(), append(base, opts...)...)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, sink
}

type logSink struct {
	mu    sync.Mutex
	lines []string
}

func (s *logSink) Printf(format string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, fmt.Sprintf(format, args...))
}

func (s *logSink) contains(substr string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, l := range s.lines {
		if strings.Contains(l, substr) {
			return true
		}
	}
	return false
}

// mkWorkspace creates a plausible gitops-style clone dir under parent and
// returns its path. It puts a file inside so a failed RemoveAll would be
// obvious.
func mkWorkspace(t *testing.T, parent, name string) string {
	t.Helper()
	p := filepath.Join(parent, name)
	if err := os.MkdirAll(filepath.Join(p, "head"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(p, "head", "App.mpr"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

// age backdates a path's mtime (and, if present, its heartbeat file's) by d.
func age(t *testing.T, path string, d time.Duration) {
	t.Helper()
	when := time.Now().Add(-d)
	if err := os.Chtimes(path, when, when); err != nil {
		t.Fatal(err)
	}
	hb := filepath.Join(path, heartbeatFile)
	if fileExists(hb) {
		if err := os.Chtimes(hb, when, when); err != nil {
			t.Fatal(err)
		}
	}
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	return err == nil
}

// --------------------------------------------------------------------------
// New / instance dir / startup sweep
// --------------------------------------------------------------------------

func TestNew_CreatesInstanceDirWithOwner(t *testing.T) {
	root := t.TempDir()
	m, err := New(root, WithSweepInterval(0), WithLogf(func(string, ...any) {}))
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	if m.Dir() != filepath.Join(root, instanceID()) {
		t.Errorf("Dir() = %q, want %q", m.Dir(), filepath.Join(root, instanceID()))
	}
	fi, err := os.Stat(m.Dir())
	if err != nil || !fi.IsDir() {
		t.Fatalf("instance dir not created: %v", err)
	}
	owner, err := os.ReadFile(filepath.Join(m.Dir(), ownerFile))
	if err != nil {
		t.Fatalf("read .owner: %v", err)
	}
	if !strings.Contains(string(owner), strconv.Itoa(os.Getpid())) {
		t.Errorf(".owner missing pid: %q", owner)
	}
}

func TestNew_StartupSweepRemovesOwnLeftovers(t *testing.T) {
	root := t.TempDir()
	// Simulate a previous run of THIS instance that died mid-request.
	own := filepath.Join(root, instanceID())
	stale := mkWorkspace(t, own, "clone-both-123")
	staleClone := mkWorkspace(t, own, "clone-456")
	keep := filepath.Join(own, "notes") // not a clone-* dir
	if err := os.MkdirAll(keep, 0o755); err != nil {
		t.Fatal(err)
	}

	if _, err := New(root, WithSweepInterval(0), WithLogf(func(string, ...any) {})); err != nil {
		t.Fatalf("New: %v", err)
	}

	if exists(t, stale) {
		t.Errorf("startup sweep left %s", stale)
	}
	if exists(t, staleClone) {
		t.Errorf("startup sweep left %s", staleClone)
	}
	if !exists(t, keep) {
		t.Errorf("startup sweep removed a non-workspace dir %s", keep)
	}
}

func TestNew_StartupSweepLeavesOtherInstancesAlone(t *testing.T) {
	root := t.TempDir()
	other := filepath.Join(root, "some-other-host-999")
	if err := os.MkdirAll(other, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(other, ownerFile), []byte("instance some-other-host-999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	live := mkWorkspace(t, other, "clone-both-abc")

	if _, err := New(root, WithSweepInterval(0), WithLogf(func(string, ...any) {})); err != nil {
		t.Fatalf("New: %v", err)
	}

	if !exists(t, live) {
		t.Errorf("startup sweep reached into another instance's dir and removed %s", live)
	}
}

// --------------------------------------------------------------------------
// reap
// --------------------------------------------------------------------------

func TestReap_RemovesIdleWorkspaceInOwnDir(t *testing.T) {
	m, sink := quietManager(t, WithTTL(30*time.Minute))

	old := mkWorkspace(t, m.Dir(), "clone-both-old")
	fresh := mkWorkspace(t, m.Dir(), "clone-both-fresh")
	age(t, old, time.Hour)

	m.reap()

	if exists(t, old) {
		t.Errorf("reap kept an idle workspace %s", old)
	}
	if !exists(t, fresh) {
		t.Errorf("reap removed a fresh workspace %s", fresh)
	}
	if !sink.contains("reclaimed") {
		t.Errorf("reap did not log the reclaim: %v", sink.lines)
	}
}

func TestReap_ReclaimsDeadInstanceWorkspaces(t *testing.T) {
	m, _ := quietManager(t, WithTTL(30*time.Minute))

	dead := filepath.Join(m.root, "dead-host-1")
	if err := os.MkdirAll(dead, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dead, ownerFile), []byte("instance dead-host-1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	oldOrphan := mkWorkspace(t, dead, "clone-both-x")
	age(t, oldOrphan, 2*time.Hour)

	m.reap()

	if exists(t, oldOrphan) {
		t.Errorf("reap did not reclaim a dead instance's idle workspace %s", oldOrphan)
	}
}

func TestReap_IgnoresDirsWithoutOwnerMarker(t *testing.T) {
	m, _ := quietManager(t, WithTTL(30*time.Minute))

	// A directory in the shared root that is NOT one of ours, holding
	// something that merely looks like a workspace. Must be untouched.
	notOurs := filepath.Join(m.root, "someone-elses-tmpdir")
	if err := os.MkdirAll(notOurs, 0o755); err != nil {
		t.Fatal(err)
	}
	decoy := mkWorkspace(t, notOurs, "clone-not-a-real-one")
	age(t, decoy, 24*time.Hour)

	m.reap()

	if !exists(t, decoy) {
		t.Errorf("reap removed %s inside a dir with no .owner marker", decoy)
	}
}

func TestReap_SkipsTrackedWorkspace(t *testing.T) {
	m, _ := quietManager(t, WithTTL(30*time.Minute), WithHeartbeatInterval(time.Hour))

	ws := mkWorkspace(t, m.Dir(), "clone-both-live")
	age(t, ws, time.Hour) // old on disk...
	stop := m.Track(ws)   // ...but a request holds it

	m.reap()
	if !exists(t, ws) {
		t.Fatalf("reap removed a tracked workspace %s", ws)
	}

	stop()
	// Track wrote a heartbeat; backdate everything again so age > ttl.
	age(t, ws, time.Hour)
	m.reap()
	if exists(t, ws) {
		t.Errorf("reap kept %s after it was released and aged", ws)
	}
}

func TestReap_HeartbeatFileKeepsWorkspaceAlive(t *testing.T) {
	m, _ := quietManager(t, WithTTL(30*time.Minute))

	ws := mkWorkspace(t, m.Dir(), "clone-both-hb")
	age(t, ws, time.Hour) // dir mtime is old

	// A fresh heartbeat file, as the beat goroutine would leave it — but not
	// tracked in the live set, so newest() is the only thing protecting it.
	if err := os.WriteFile(filepath.Join(ws, heartbeatFile), []byte(time.Now().Format(time.RFC3339)), 0o644); err != nil {
		t.Fatal(err)
	}

	m.reap()
	if !exists(t, ws) {
		t.Errorf("reap ignored a fresh heartbeat and removed %s", ws)
	}

	age(t, ws, time.Hour) // now backdate the heartbeat too
	m.reap()
	if exists(t, ws) {
		t.Errorf("reap kept %s after its heartbeat went stale", ws)
	}
}

// --------------------------------------------------------------------------
// Track
// --------------------------------------------------------------------------

func TestTrack_NilManagerIsNoop(t *testing.T) {
	var m *Manager
	stop := m.Track("/anything")
	stop() // must not panic
}

func TestTrack_WritesAndRefreshesHeartbeat(t *testing.T) {
	m, _ := quietManager(t, WithHeartbeatInterval(15*time.Millisecond))

	ws := mkWorkspace(t, m.Dir(), "clone-both-beat")
	stop := m.Track(ws)
	defer stop()

	hb := filepath.Join(ws, heartbeatFile)
	// First write happens synchronously inside Track's goroutine start; give
	// it a moment, then confirm it exists and is refreshed at least once.
	waitFor(t, time.Second, func() bool { return fileExists(hb) })

	first := statModTime(hb)
	waitFor(t, time.Second, func() bool { return statModTime(hb).After(first) })
}

func TestTrack_StopIsIdempotent(t *testing.T) {
	m, _ := quietManager(t, WithHeartbeatInterval(10*time.Millisecond))
	ws := mkWorkspace(t, m.Dir(), "clone-both-idem")

	stop := m.Track(ws)
	stop()
	stop() // second call must be a no-op, not a double close panic

	m.mu.Lock()
	_, stillLive := m.active[ws]
	m.mu.Unlock()
	if stillLive {
		t.Errorf("workspace still in live set after stop")
	}
}

// --------------------------------------------------------------------------
// StartJanitor
// --------------------------------------------------------------------------

func TestStartJanitor_ReapsThenStopsOnCancel(t *testing.T) {
	root := t.TempDir()
	m, err := New(root,
		WithTTL(30*time.Minute),
		WithSweepInterval(10*time.Millisecond),
		WithLogf(func(string, ...any) {}),
	)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	old := mkWorkspace(t, m.Dir(), "clone-both-1")
	age(t, old, time.Hour)

	ctx, cancel := context.WithCancel(context.Background())
	m.StartJanitor(ctx)

	waitFor(t, 2*time.Second, func() bool { return !exists(t, old) })

	cancel()
	time.Sleep(50 * time.Millisecond) // let the goroutine observe cancellation

	// After cancel the janitor must be inert.
	old2 := mkWorkspace(t, m.Dir(), "clone-both-2")
	age(t, old2, time.Hour)
	time.Sleep(100 * time.Millisecond)
	if !exists(t, old2) {
		t.Errorf("janitor still running after context cancel")
	}
}

func TestStartJanitor_NoopWhenDisabledOrNil(t *testing.T) {
	var nilM *Manager
	nilM.StartJanitor(context.Background()) // must not panic

	m, _ := quietManager(t) // WithSweepInterval(0)
	m.StartJanitor(context.Background())
	old := mkWorkspace(t, m.Dir(), "clone-both-x")
	age(t, old, time.Hour)
	time.Sleep(50 * time.Millisecond)
	if !exists(t, old) {
		t.Errorf("a disabled janitor reaped %s", old)
	}
}

// --------------------------------------------------------------------------
// misc
// --------------------------------------------------------------------------

func TestDescribe_MentionsIdAndTTL(t *testing.T) {
	m, _ := quietManager(t, WithTTL(42*time.Minute))
	d := m.Describe()
	if !strings.Contains(d, instanceID()) || !strings.Contains(d, "42m") {
		t.Errorf("Describe() = %q", d)
	}
}

func waitFor(t *testing.T, limit time.Duration, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(limit)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %s", limit)
}
