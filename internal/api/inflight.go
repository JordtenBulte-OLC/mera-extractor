// internal/api/inflight.go
package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"runtime/debug"
	"sync"
)

// extractFlights collapses identical /extract requests that are in flight at
// the same time: the second caller waits for the first one's run instead of
// starting its own clone and mx pipeline.
//
// ▶ Why: a caller that times out and retries while the first attempt is
// still running doubles the load on the exact resource that made it slow.
// On 2026-10-30 four overlapping attempts for the same commit pair all split
// one ~90 KB/s Team Server link, and every one of them hit the 5-minute wall.
// Real reviews rarely ask for the same pair twice at once. Testing and retry
// loops do.
//
// The zero value is ready to use.
type extractFlights struct {
	mu sync.Mutex
	m  map[string]*extractFlight
}

// extractFlight is one shared run. out is written once, before done closes,
// and only read after it.
type extractFlight struct {
	leaderRID string
	stages    *stageTimer
	done      chan struct{}
	out       extractOutcome

	waiters int // guarded by extractFlights.mu
	cancel  context.CancelFunc
}

// extractOutcome is a finished run as data rather than as bytes on one
// ResponseWriter, so that every caller sharing it can write its own copy.
type extractOutcome struct {
	status int   // HTTP status when err != nil
	err    error // nil means resp is a 200
	resp   extractResponse
	fields []any // annotate key/value pairs for each caller's "←" line
}

// statusClientClosedRequest is nginx's convention for "the caller hung up
// before we answered". Nobody receives the response. It exists so the "←"
// line says what happened instead of a status that looks like our fault.
const statusClientClosedRequest = 499

// extractKey identifies requests that may share one run.
//
// ▶ Why the credentials are part of the key: the result is only proof that
// the CALLER'S credentials can read the repo if the caller's credentials
// fetched it. Keying on repo+SHAs alone would let a request with a wrong or
// missing PAT ride along on someone else's clone and receive the model.
// Hashed, so the key never holds the PAT in plain text.
func extractKey(req extractRequest) string {
	h := sha256.New()
	for _, part := range []string{req.RepoURL, req.Username, req.Pat, req.BaseSha, req.HeadSha} {
		h.Write([]byte(part))
		h.Write([]byte{0}) // separator: ("ab","c") and ("a","bc") must differ
	}
	return hex.EncodeToString(h.Sum(nil))
}

// join registers the caller on the run for key, starting fn if no run is in
// flight, and reports whether it found one already going. Every join must be
// followed by exactly one wait. Joining and waiting are separate steps so the
// caller can log "joined" the moment it happens. A shared run can take
// minutes, and until it finishes the log is the only way to see two requests
// sharing one clone.
//
// The run's context is NOT any one caller's request context. It is
// cancelled only when every caller waiting on it has gone.
//
// ▶ Why: with the first caller's context, the first caller hanging up would
// cancel the run under everyone who joined later, and they would all fail
// for a reason that has nothing to do with them. Cancelling when the LAST
// waiter leaves keeps the old single-caller behaviour exactly: a caller that
// gives up still stops the git and mx subprocesses it caused.
func (fs *extractFlights) join(rid, key string, fn func(ctx context.Context, stages *stageTimer) extractOutcome) (fl *extractFlight, joined bool) {
	fs.mu.Lock()
	if fs.m == nil {
		fs.m = map[string]*extractFlight{}
	}
	fl, joined = fs.m[key]
	if !joined {
		runCtx, cancel := context.WithCancel(context.Background())
		fl = &extractFlight{leaderRID: rid, stages: &stageTimer{}, done: make(chan struct{}), cancel: cancel}
		fs.m[key] = fl
		go fs.run(runCtx, key, fl, fn)
	}
	fl.waiters++
	fs.mu.Unlock()
	return fl, joined
}

// wait blocks until fl's run finishes or ctx is done, whichever is first.
func (fs *extractFlights) wait(ctx context.Context, key string, fl *extractFlight) (extractOutcome, error) {
	select {
	case <-fl.done:
		return fl.out, nil
	case <-ctx.Done():
		fs.mu.Lock()
		fl.waiters--
		if fl.waiters == 0 {
			// Unregister now, not when the run winds down: a caller arriving
			// in between must start a fresh run, not join a cancelled one.
			fs.forgetLocked(key, fl)
			fl.cancel()
		}
		fs.mu.Unlock()
		return extractOutcome{}, ctx.Err()
	}
}

func (fs *extractFlights) run(ctx context.Context, key string, fl *extractFlight, fn func(context.Context, *stageTimer) extractOutcome) {
	defer close(fl.done)
	defer fl.cancel()
	defer func() {
		fs.mu.Lock()
		fs.forgetLocked(key, fl)
		fs.mu.Unlock()
	}()
	// ▶ Why recover here: this goroutine is not a handler, so net/http's
	// per-request recovery does not cover it. A panic in any extraction step
	// would otherwise kill the whole server and every other request in it.
	defer func() {
		if p := recover(); p != nil {
			log.Printf("http rid=%s PANIC in shared extract run: %v\n%s", fl.leaderRID, p, debug.Stack())
			fl.out = extractOutcome{status: http.StatusInternalServerError, err: fmt.Errorf("panic: %v", p)}
		}
	}()
	fl.out = fn(ctx, fl.stages)
}

// forgetLocked removes fl from the map, but only if it is still the flight
// registered under key. A newer run may have taken the slot after fl was
// abandoned.
func (fs *extractFlights) forgetLocked(key string, fl *extractFlight) {
	if fs.m[key] == fl {
		delete(fs.m, key)
	}
}
