// internal/mx/mx.go
package mx

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"
)

// result is what run() hands back to every internal/mx caller.
//
// Unlike internal/mxcli's run(), which treats any non-zero exit as failure
// and folds stderr into an error, this run() does NOT decide success or
// failure by exit code. Each mx subcommand defines its own exit-code table
// (mx diff: 0 OK / 2 conflicts-still-usable / 4 unsupported version / 129
// generic error; mx dump-mpr: 0 OK / 1 wrong project file / 2 invalid unit
// type(s) / 3 unknown JSON export error / 4 different Mendix version — a
// DIFFERENT table, not to be conflated with diff's) — so only the caller
// (Analyze, Diff, ResolveQualifiedNames) knows what a given code means.
type result struct {
	Stdout   string
	Stderr   string
	ExitCode int
}

// run executes bin.Path — a specific, already-resolved mx binary, never a
// bare "mx" on PATH, since more than one Mendix version's mx can be
// installed side by side under mxRoot (see version.go) — with a bounded
// timeout, and returns stdout/stderr/exit code separately.
//
// It returns a non-nil error only for a real execution failure: the binary
// couldn't be started, or ctx was cancelled/timed out before the process
// finished. A non-zero exit that the process itself reported is NOT an
// error here — it comes back as a normal ExitCode for the caller to switch
// on. This also sidesteps a real gotcha this project already hit with
// fetch-mx.sh: this mx build's exit code for some usage cases (e.g.
// --help) doesn't reliably mean "failure" — treating exit code as data,
// not as success/failure, avoids depending on that behavior being sane.
//
// No global concurrency semaphore here (unlike mxcli.run()) — mx is called
// a handful of times per request, not once per unit, so there's no fan-out
// to bound. No banner-stripping either — nothing captured from this mx
// build's output so far shows one; add it here, the same way mxcli.go
// does, if that ever turns out to be wrong.
func run(ctx context.Context, bin Binary, timeout time.Duration, args ...string) (result, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin.Path, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return result{
				Stdout:   stdout.String(),
				Stderr:   stderr.String(),
				ExitCode: ee.ExitCode(),
			}, nil
		}
		// Not an ExitError: the process never produced an exit code at
		// all — binary not found, permission denied, ctx deadline
		// exceeded, ctx cancelled. This is the one case worth failing on.
		return result{}, fmt.Errorf("mx %v: %w", args, err)
	}
	return result{Stdout: stdout.String(), Stderr: stderr.String(), ExitCode: 0}, nil
}

// stripBOM removes a leading UTF-8 byte-order-mark, if present. mx is a
// .NET tool, and .NET's default file-writing APIs emit a UTF-8 BOM unless
// explicitly told not to — confirmed real: a real mx diff output file came
// back with a leading '\ufeff' that encoding/json refuses outright
// ("invalid character '\ufeff' looking for beginning of value"). Applied
// everywhere internal/mx reads JSON mx wrote to a file (diff.go, resolve.go)
// and defensively to analyze-mpr's stdout text too, since the same .NET
// runtime produces both.
func stripBOM(data []byte) []byte {
	return bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF})
}