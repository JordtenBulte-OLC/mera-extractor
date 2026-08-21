// internal/mxcli/mxcli.go
package mxcli

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

var bannerPrefixes = []string{
	"WARNING: This is a vibe-coded PoC",
	"Connected to:",
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