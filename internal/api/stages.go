// internal/api/stages.go
package api

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// stageTimer records how long each step of an /extract took, for the "←"
// access-log line: stages=clone:41.2s,prepare:3.1s,diff:5.0s,...
//
// ▶ Why: until 2026-10-30 the log carried only the total. A request that hit
// the caller's 5-minute timeout said "dur=5m0s" and nothing about where the
// five minutes went, so finding out took live process inspection inside the
// container. It was a Team Server blob fetch in the base checkout. With this,
// the log line alone answers it.
//
// A stage still running when the line is written shows as "(unfinished)":
// a request killed by its caller reports the stage it died in, which is the
// one fact that matters most in that case.
//
// Safe for concurrent use. A collapsed /extract (see inflight.go) is read by
// every waiting caller while the shared run is still writing to it.
type stageTimer struct {
	mu      sync.Mutex
	done    []stageTime
	current string
	started time.Time
}

type stageTime struct {
	name string
	dur  time.Duration
}

// start ends the running stage, if any, and begins name.
func (t *stageTimer) start(name string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endLocked()
	t.current, t.started = name, time.Now()
}

// stop ends the running stage. Idempotent, so a deferred stop after an early
// return is always safe.
func (t *stageTimer) stop() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.endLocked()
}

func (t *stageTimer) endLocked() {
	if t.current == "" {
		return
	}
	t.done = append(t.done, stageTime{t.current, time.Since(t.started)})
	t.current = ""
}

// String renders the stages in the order they ran. Durations are rounded
// to 100ms: the stages that matter take seconds to minutes.
func (t *stageTimer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	parts := make([]string, 0, len(t.done)+1)
	for _, s := range t.done {
		parts = append(parts, fmt.Sprintf("%s:%s", s.name, s.dur.Round(100*time.Millisecond)))
	}
	if t.current != "" {
		parts = append(parts, fmt.Sprintf("%s:%s(unfinished)",
			t.current, time.Since(t.started).Round(100*time.Millisecond)))
	}
	return strings.Join(parts, ",")
}
