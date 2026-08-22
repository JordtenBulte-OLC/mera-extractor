// internal/mxcli/concurrency_test.go
package mxcli

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// acquireForTest exercises the exact same gating pattern run() uses —
// acquire a slot from the package-level sem, defer the release — without
// shelling out to a real mxcli binary. This isolates the semaphore
// mechanism (what Stage 7 changed) from process execution (unchanged
// since Stage 5, and not testable without the real binary anyway).
func acquireForTest(work func()) {
	sem <- struct{}{}
	defer func() { <-sem }()
	work()
}

func TestSetMaxConcurrent_BoundsGlobally(t *testing.T) {
	SetMaxConcurrent(4)

	var inFlight, maxInFlight int32
	track := func() {
		cur := atomic.AddInt32(&inFlight, 1)
		defer atomic.AddInt32(&inFlight, -1)
		for {
			old := atomic.LoadInt32(&maxInFlight)
			if cur <= old || atomic.CompareAndSwapInt32(&maxInFlight, old, cur) {
				break
			}
		}
		time.Sleep(15 * time.Millisecond)
	}

	// Simulate TWO overlapping "requests" (e.g. two /extract calls), each
	// firing its own burst of concurrent work — the exact scenario a
	// per-request pool handled wrong: each pool would enforce its own
	// separate limit of 4, and the two together could reach 8 real mxcli
	// processes even though the machine only has 4 cores. A single shared
	// semaphore, gated inside mxcli.run(), must hold the line at 4 no
	// matter how many logical "requests" are contributing work.
	var wg sync.WaitGroup
	launchBurst := func(n int) {
		for i := 0; i < n; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				acquireForTest(track)
			}()
		}
	}
	launchBurst(10) // "request A": 10 units
	launchBurst(10) // "request B": 10 units, overlapping with A

	wg.Wait()

	max := atomic.LoadInt32(&maxInFlight)
	if max > 4 {
		t.Fatalf("global semaphore let %d calls run at once across two overlapping bursts, want <= 4", max)
	}
	if max < 2 {
		t.Fatalf("only %d call(s) ever ran concurrently — not parallelizing at all", max)
	}
	t.Logf("observed max concurrent across two overlapping bursts of 10: %d (limit was 4)", max)
}

func TestSetMaxConcurrent_DefaultIsPositive(t *testing.T) {
	// The package-level default (runtime.NumCPU()) must never produce a
	// zero-capacity channel — that would deadlock every caller of run().
	if cap(sem) < 1 {
		t.Fatalf("default sem capacity is %d, want >= 1", cap(sem))
	}
}