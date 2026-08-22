// internal/mxcli/mxcli.go
package mxcli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

var bannerPrefixes = []string{
	"WARNING: This is a vibe-coded PoC",
	"Connected to:",
}

// sem bounds how many mxcli subprocesses may run concurrently across the
// whole process — not per request, not per caller. Every function in this
// package that shells out funnels through run(), so gating here gates
// everything: Describe, ListUnits, Version, called from any handler, in
// any combination, at once. That's what makes it a real global cap instead
// of a per-request one that multiplies with concurrent requests.
//
// Defaults to runtime.NumCPU() — see SetMaxConcurrent for the container
// caveat on that default.
var sem = make(chan struct{}, runtime.NumCPU())
 
// SetMaxConcurrent replaces the global subprocess concurrency limit.
//
// Call this exactly once, at startup, before the server begins accepting
// requests. It swaps the semaphore channel outright; any run() call already
// blocked waiting on the old channel would never see the new one and would
// hang until process exit. That's not a runtime hazard in normal use —
// main.go calls this before http.ListenAndServe, and nothing calls run()
// before that — but it's why this is a startup-time knob, not something to
// expose as a live-reloadable setting later without redesigning it (e.g.
// swapping to an atomic.Pointer if that need ever arises).
//
// runtime.NumCPU() (the package-level default) reports the host's visible
// logical CPU count. Inside a container with a CPU quota below the host's
// full capacity (an Azure Container Apps/Fargate/Cloud Run `--cpus` limit
// smaller than the node it's scheduled on), that can overstate what's
// actually available to this process — pass the real quota explicitly via
// an env var in that case rather than relying on the default.
func SetMaxConcurrent(n int) {
	if n < 1 {
		n = 1
	}
	sem = make(chan struct{}, n)
}

func run(ctx context.Context, timeout time.Duration, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "mxcli", args...)
	out, err := cmd.Output()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && len(ee.Stderr) > 0 {
			return "", fmt.Errorf("mxcli %v: %s", args, string(ee.Stderr))
		}
		return "", fmt.Errorf("mxcli %v: %w", args, err)
	}
	return stripBanner(string(out)), nil
}

// stripBanner removes mxcli's startup banner lines from stdout, before any
// caller sees them. There's no flag that suppresses this at the source —
// --quiet only exists on `search`, and the global --json flag doesn't
// suppress it either (confirmed: it printed even with --json set).
//
// This matches against alpha software's actual banner text, not a stable
// API — if mxcli's wording changes in a future release this may need
// updating. If a real suppression flag ever appears in `mxcli --help`,
// prefer that over this.
func stripBanner(output string) string {
	lines := strings.Split(output, "\n")
	i := 0
	for i < len(lines) {
		trimmed := strings.TrimSpace(lines[i])
		matched := false
		for _, prefix := range bannerPrefixes {
			if strings.HasPrefix(trimmed, prefix) {
				matched = true
				break
			}
		}
		if !matched {
			break
		}
		i++
	}
	return strings.Join(lines[i:], "\n")
}