// internal/mx/diff_test.go
package mx

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDiffStubMx creates a stub mx whose "diff" subcommand writes diffJSON
// to the outPath argument — mx diff's 4th positional arg, matching the real
// "mx diff BASE MINE OUTPUT" usage — mirroring that mx diff writes its
// result to a file, not stdout. Also emits stderr and exits with exitCode.
// Skips writing outPath entirely when diffJSON is "", to simulate exit
// codes where mx diff doesn't produce usable output (4/129/unexpected).
func writeDiffStubMx(t *testing.T, diffJSON, stderr string, exitCode int) Binary {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mx")

	script := "#!/bin/sh\n" + `outPath="$4"` + "\n"
	if diffJSON != "" {
		script += "cat > \"$outPath\" <<'MERA_DIFF_EOF'\n" + diffJSON + "\nMERA_DIFF_EOF\n"
	}
	if stderr != "" {
		script += "cat <<'MERA_STDERR_EOF' >&2\n" + stderr + "\nMERA_STDERR_EOF\n"
	}
	script += fmt.Sprintf("exit %d\n", exitCode)

	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("writeDiffStubMx: %v", err)
	}
	return Binary{Version: "stub", Path: path}
}

const sampleDiffJSON = `{
  "unitDifferences": [
    {"type":"Microflows$Microflow","id":"da17a44c-9998-4714-8bcc-4e2bed3d7b79","change":"Added","containerId":"5f2577c0-1b6e-444d-b6af-f2f59b458b4f","containmentName":"Documents"}
  ]
}`

func TestDiff_ExitCode0_Success(t *testing.T) {
	bin := writeDiffStubMx(t, sampleDiffJSON, "", 0)
	outPath := filepath.Join(t.TempDir(), "diff.json")

	res, err := Diff(context.Background(), bin, "base.mpr", "head.mpr", outPath)
	if err != nil {
		t.Fatalf("Diff: unexpected error: %v", err)
	}
	if res.ConflictsFound {
		t.Error("ConflictsFound = true, want false for exit 0")
	}
	if len(res.UnitDifferences) != 1 {
		t.Fatalf("len(UnitDifferences) = %d, want 1", len(res.UnitDifferences))
	}
	got := res.UnitDifferences[0]
	if got.ID != "da17a44c-9998-4714-8bcc-4e2bed3d7b79" || got.Type != "Microflows$Microflow" || got.Change != "Added" {
		t.Errorf("UnitDifferences[0] = %+v, unexpected", got)
	}
}

func TestDiff_ExitCode2_ConflictsStillUsable(t *testing.T) {
	bin := writeDiffStubMx(t, sampleDiffJSON, "", 2)
	outPath := filepath.Join(t.TempDir(), "diff.json")

	res, err := Diff(context.Background(), bin, "base.mpr", "head.mpr", outPath)
	if err != nil {
		t.Fatalf("Diff: unexpected error: %v", err)
	}
	if !res.ConflictsFound {
		t.Error("ConflictsFound = false, want true for exit 2")
	}
	if len(res.UnitDifferences) != 1 {
		t.Fatalf("len(UnitDifferences) = %d, want 1", len(res.UnitDifferences))
	}
}

func TestDiff_ExitCode4_UnsupportedVersion(t *testing.T) {
	bin := writeDiffStubMx(t, "", "unsupported .mpr version", 4)
	outPath := filepath.Join(t.TempDir(), "diff.json")

	_, err := Diff(context.Background(), bin, "base.mpr", "head.mpr", outPath)
	if err == nil {
		t.Fatal("Diff returned no error for exit 4")
	}
	var target *ErrUnsupportedMendixVersion
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *ErrUnsupportedMendixVersion", err)
	}
	if !strings.Contains(target.Stderr, "unsupported .mpr version") {
		t.Errorf("Stderr = %q, missing expected text", target.Stderr)
	}
}

func TestDiff_ExitCode129_DiffFailed(t *testing.T) {
	bin := writeDiffStubMx(t, "", "generic diff failure", 129)
	outPath := filepath.Join(t.TempDir(), "diff.json")

	_, err := Diff(context.Background(), bin, "base.mpr", "head.mpr", outPath)
	if err == nil {
		t.Fatal("Diff returned no error for exit 129")
	}
	var target *ErrDiffFailed
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *ErrDiffFailed", err)
	}
}

func TestDiff_UnexpectedExitCode(t *testing.T) {
	bin := writeDiffStubMx(t, "", "something weird", 7)
	outPath := filepath.Join(t.TempDir(), "diff.json")

	_, err := Diff(context.Background(), bin, "base.mpr", "head.mpr", outPath)
	if err == nil {
		t.Fatal("Diff returned no error for an unrecognized exit code")
	}
	var target *ErrUnexpectedExitCode
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *ErrUnexpectedExitCode", err)
	}
	if target.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", target.ExitCode)
	}
}

func TestDiff_RealExecutionFailure(t *testing.T) {
	bin := Binary{Version: "missing", Path: "/definitely/does/not/exist/mx"}
	outPath := filepath.Join(t.TempDir(), "diff.json")

	_, err := Diff(context.Background(), bin, "base.mpr", "head.mpr", outPath)
	if err == nil {
		t.Fatal("Diff returned no error for a nonexistent binary")
	}
	var unexpected *ErrUnexpectedExitCode
	if errors.As(err, &unexpected) {
		t.Error("a real execution failure should not be typed as ErrUnexpectedExitCode")
	}
}

func TestDiff_MalformedJSON(t *testing.T) {
	bin := writeDiffStubMx(t, "{not valid json", "", 0)
	outPath := filepath.Join(t.TempDir(), "diff.json")

	_, err := Diff(context.Background(), bin, "base.mpr", "head.mpr", outPath)
	if err == nil {
		t.Fatal("Diff returned no error for malformed diff.json")
	}
}

func TestDiff_MissingOutputFile(t *testing.T) {
	// exit 0 but the stub never actually wrote outPath — simulates a
	// real-world "mx claimed success but the file isn't there" case.
	bin := writeDiffStubMx(t, "", "", 0)
	outPath := filepath.Join(t.TempDir(), "diff.json")

	_, err := Diff(context.Background(), bin, "base.mpr", "head.mpr", outPath)
	if err == nil {
		t.Fatal("Diff returned no error when outPath was never written")
	}
}

func TestDiff_RealCapturedFixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/diff-image-and-microflow.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	bin := writeDiffStubMx(t, string(fixture), "", 0)
	outPath := filepath.Join(t.TempDir(), "diff.json")

	res, err := Diff(context.Background(), bin, "base.mpr", "head.mpr", outPath)
	if err != nil {
		t.Fatalf("Diff: unexpected error: %v", err)
	}
	if len(res.UnitDifferences) != 2 {
		t.Fatalf("len(UnitDifferences) = %d, want 2", len(res.UnitDifferences))
	}

	img := res.UnitDifferences[0]
	if img.Type != "Images$ImageCollection" || img.Change != "Modified" || img.ID != "652106e9-3f38-46c7-bcc4-2b4e4ad39160" {
		t.Errorf("UnitDifferences[0] = %+v, unexpected", img)
	}
	if !strings.Contains(string(img.Raw), "changeDetails") {
		t.Error("Raw does not preserve changeDetails for the Modified unit — StructuralDelta (architecture §2) needs this")
	}

	mf := res.UnitDifferences[1]
	if mf.Type != "Microflows$Microflow" || mf.Change != "Added" || mf.ID != "da17a44c-9998-4714-8bcc-4e2bed3d7b79" {
		t.Errorf("UnitDifferences[1] = %+v, unexpected", mf)
	}
}

func TestDiff_HandlesUTF8BOM(t *testing.T) {
	bomPrefixed := "\ufeff" + sampleDiffJSON
	bin := writeDiffStubMx(t, bomPrefixed, "", 0)
	outPath := filepath.Join(t.TempDir(), "diff.json")

	res, err := Diff(context.Background(), bin, "base.mpr", "head.mpr", outPath)
	if err != nil {
		t.Fatalf("Diff: unexpected error with BOM-prefixed output: %v", err)
	}
	if len(res.UnitDifferences) != 1 {
		t.Fatalf("len(UnitDifferences) = %d, want 1", len(res.UnitDifferences))
	}
}