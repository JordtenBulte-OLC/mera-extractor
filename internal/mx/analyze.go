// internal/mx/analyze.go
package mx

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type AnalyzeResult struct {
	MendixVersion        string
	HasProjectConversion bool // Projects$ProjectConversion present → migration commit, unsupported
	Raw                  string
}

// ErrUnsupportedVersionMigrationCommit indicates the .mpr snapshot being
// analyzed is mid Studio-Pro-version migration (a Projects$ProjectConversion
// unit is present) — no mx build parses this cleanly (trap #16, confirmed
// this project: the "$ID"/"Associations" parse exception that first
// surfaced turned out to be exactly this, not a real mx bug). Callers
// should check AnalyzeResult.HasProjectConversion and return this
// immediately, rather than continuing on to Diff/dump-mpr and letting a
// raw parse exception surface instead.
//
// Not yet wired into any caller — internal/api/extract.go's handleExtract
// is the intended call site (Step 7), which hasn't landed. Defined here now
// so Step 7 has a ready-made typed error rather than inventing one then.
type ErrUnsupportedVersionMigrationCommit struct {
	MendixVersion string
}

func (e *ErrUnsupportedVersionMigrationCommit) Error() string {
	return fmt.Sprintf("mx: .mpr is mid version-migration (Mendix version %s) — not supported", e.MendixVersion)
}

// Analyze runs `mx analyze-mpr` and extracts the Mendix version plus
// whether this snapshot is mid Studio-Pro-version migration.
//
// analyze-mpr is confirmed version-agnostic — a single mx build correctly
// read an 11.10.0-authored file during this project's own $ID/Associations
// investigation — so this is meant to be called with whatever Highest()
// returns, BEFORE the real version-matched binary is even known. That
// resolves the chicken-and-egg problem of "need the version to pick a
// binary, need a binary to read the version."
func Analyze(ctx context.Context, bin Binary, mprPath string) (AnalyzeResult, error) {
	res, err := run(ctx, bin, 30*time.Second, "analyze-mpr", mprPath)
	if err != nil {
		return AnalyzeResult{}, err // real execution failure
	}
	if res.ExitCode != 0 {
		// analyze-mpr has no documented multi-code table like diff/dump-mpr
		// do — until real testing says otherwise, treat any non-zero here
		// as a generic failure, stderr included.
		return AnalyzeResult{}, fmt.Errorf("mx analyze-mpr: exit %d: %s", res.ExitCode, res.Stderr)
	}
	return parseAnalyzeOutput(res.Stdout), nil
}

// parseAnalyzeOutput extracts what Analyze needs from analyze-mpr's plain
// text output: the "Mendix version: X" line, and whether a
// Projects$ProjectConversion unit is present anywhere in the output.
// Deliberately tolerant — this is scanning free-form CLI text, not a
// documented format — so it degrades to an empty MendixVersion rather than
// erroring if the expected line isn't found verbatim.
func parseAnalyzeOutput(stdout string) AnalyzeResult {
	stdout = strings.TrimPrefix(stdout, "\ufeff")
	res := AnalyzeResult{Raw: stdout}
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)
		if v, ok := strings.CutPrefix(trimmed, "Mendix version:"); ok {
			res.MendixVersion = strings.TrimSpace(v)
		}
	}
	res.HasProjectConversion = strings.Contains(stdout, "Projects$ProjectConversion")
	return res
}