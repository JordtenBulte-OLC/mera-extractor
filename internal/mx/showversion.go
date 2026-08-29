// internal/mx/showversion.go
package mx

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// showVersionTimeout: `mx show-version` does a single SQLite metadata read
// (DatabaseManager.RetrieveMetadata), no model walk — it returns well under a
// second on a real app. 30s matches Analyze's old budget and leaves generous
// headroom for a cold .NET start.
const showVersionTimeout = 30 * time.Second

// ShowVersion runs `mx show-version <mpr>` and returns the Studio Pro version
// the app was last edited with — e.g. "11.13.0", spelled exactly the way
// Resolve expects it (it matches the /opt/mx/<version> directory names).
//
// ▶ Why this and not analyze-mpr, which PrepareMpr used to call: analyze-mpr
// computes a full storage-stats report and, on some real models, recurses
// without bound in MprStats and aborts the whole process with a stack
// overflow (SIGABRT, exit 134) — see MERA-session-status.md. show-version
// only reads the .mpr's SQLite metadata, so it cannot hit that path. The
// version was the only load-bearing thing PrepareMpr ever took from
// analyze-mpr; this is the surgical replacement.
//
// Two real-world quirks, both confirmed by capture against mx 11.12.1:
//   - show-version has no documented exit-code table (its --help lists only
//     "0 if Ok"), so any non-zero exit is a generic failure here.
//   - it writes its .NET error dump to STDOUT, not stderr — so the error
//     text is built from stdout first, with stderr folded in behind it.
func ShowVersion(ctx context.Context, bin Binary, mprPath string) (string, error) {
	res, err := run(ctx, bin, showVersionTimeout, "show-version", mprPath)
	if err != nil {
		return "", err // real execution failure: binary missing, timeout, ctx cancelled
	}
	if res.ExitCode != 0 {
		msg := strings.TrimSpace(res.Stdout + "\n" + res.Stderr)
		return "", fmt.Errorf("mx show-version: exit %d: %s", res.ExitCode, msg)
	}

	version := firstLine(strings.TrimSpace(string(stripBOM([]byte(res.Stdout)))))
	if version == "" {
		return "", fmt.Errorf("mx show-version: exit 0 but no version in output %q", res.Stdout)
	}
	return version, nil
}

// firstLine returns s up to (not including) the first newline, trimmed.
// show-version prints exactly one line on success; taking the first line is
// pure defence against a future build adding a banner or trailing notice.
func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	return strings.TrimSpace(s)
}
