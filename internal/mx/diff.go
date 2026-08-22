// internal/mx/diff.go
package mx

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"
)

type UnitDifference struct {
	Type            string `json:"type"`
	ID              string `json:"id"`
	Change          string `json:"change"` // Added | Modified | Deleted
	ContainerID     string `json:"containerId"`
	ContainmentName string `json:"containmentName"`
	Raw 			json.RawMessage `json:"-"`
	// Raw is the complete per-unit object exactly as mx diff produced it,
	// including changeDetails (property-level sub-changes for Modified
	// units) — changeDetails is a nested,
	// variable structure (property/baseValue/mineValue, itself recursively
	// nesting for structured properties like Images$ImageCollection's
	// added images), not worth fully typing here. Architecture doc §2's
	// ChangeUnit.StructuralDelta wants "a slice of mx diff JSON for this
	// unit" — this is exactly that slice, ready to drop into
	// StructuralDelta once Step 7 assembles ChangeUnits.

}

type DiffResult struct {
	UnitDifferences []UnitDifference
	ConflictsFound  bool // exit code 2 — still usable, per manual
}

// ErrUnsupportedMendixVersion: mx diff exited 4 — the .mpr is in a Mendix
// version this mx build doesn't support.
type ErrUnsupportedMendixVersion struct {
	Stderr string
}

func (e *ErrUnsupportedMendixVersion) Error() string {
	return fmt.Sprintf("mx diff: unsupported Mendix version: %s", e.Stderr)
}

// ErrDiffFailed: mx diff exited 129 — a generic diff error.
type ErrDiffFailed struct {
	Stderr string
}

func (e *ErrDiffFailed) Error() string {
	return fmt.Sprintf("mx diff: failed: %s", e.Stderr)
}

// ErrUnexpectedExitCode: mx diff exited with a code outside the documented
// table (0/2/4/129). Carries the raw exit code and full stderr so this can
// be logged loudly rather than silently swallowed — this project already
// found this CLI's exit-code behavior unreliable once (fetch-mx.sh's
// --help handling), so an unrecognized code is worth surfacing plainly.
type ErrUnexpectedExitCode struct {
	ExitCode int
	Stderr   string
}

func (e *ErrUnexpectedExitCode) Error() string {
	return fmt.Sprintf("mx diff: unexpected exit code %d: %s", e.ExitCode, e.Stderr)
}

// Diff runs `mx diff basePath headPath outPath` and reads back the JSON it
// writes to outPath — mx diff writes its result to that file, NOT stdout
// (mirrors dump-mpr's --output-file behavior), so run()'s captured stdout
// is not the payload here.
func Diff(ctx context.Context, bin Binary, basePath, headPath, outPath string) (DiffResult, error) {
	res, err := run(ctx, bin, 5*time.Minute, "diff", basePath, headPath, outPath)
	if err != nil {
		return DiffResult{}, err // real execution failure
	}

	switch res.ExitCode {
	case 0:
		return readDiffResult(outPath, false)
	case 2:
		return readDiffResult(outPath, true)
	case 4:
		return DiffResult{}, &ErrUnsupportedMendixVersion{Stderr: res.Stderr}
	case 129:
		return DiffResult{}, &ErrDiffFailed{Stderr: res.Stderr}
	default:
		return DiffResult{}, &ErrUnexpectedExitCode{ExitCode: res.ExitCode, Stderr: res.Stderr}
	}
}

// diffFile mirrors mx diff's output JSON shape. NOT independently
// confirmed against a real diff.json in this conversation — the plan
// document's wording ("mx diff's unitDifferences[] gives id/type/change/
// containerId/containmentName") implies a wrapper object with a
// unitDifferences key, not a bare top-level array, but verify this against
// an actual diff.json from your own testing and adjust the field name /
// unwrap a bare array here if it turns out to differ.

type diffFile struct {
	UnitDifferences []json.RawMessage `json:"unitDifferences"`
}

func readDiffResult(outPath string, conflictsFound bool) (DiffResult, error) {
	data, err := os.ReadFile(outPath)
	if err != nil {
		return DiffResult{}, fmt.Errorf("mx diff: reading %s: %w", outPath, err)
	}
	data = stripBOM(data)
	var df diffFile
	if err := json.Unmarshal(data, &df); err != nil {
		return DiffResult{}, fmt.Errorf("mx diff: parsing %s: %w", outPath, err)
	}

	diffs := make([]UnitDifference, 0, len(df.UnitDifferences))
	for _, raw := range df.UnitDifferences {
		var ud UnitDifference
		if err := json.Unmarshal(raw, &ud); err != nil {
			return DiffResult{}, fmt.Errorf("mx diff: parsing unit difference: %w", err)
		}
		ud.Raw = append(json.RawMessage(nil), raw...)
		diffs = append(diffs, ud)
	}
	return DiffResult{UnitDifferences: diffs, ConflictsFound: conflictsFound}, nil
}
