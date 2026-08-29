// internal/mx/prepare_test.go
package mx

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestParseSelfReferenceMismatch(t *testing.T) {
	cases := []struct {
		name     string
		err      error
		wantName string
		wantOK   bool
	}{
		{
			name:     "real confirmed error text",
			err:      errors.New("Cannot open MPR file MERA.mpr... existing MPR contents refer to MPR file 'App.mpr'"),
			wantName: "App.mpr",
			wantOK:   true,
		},
		{name: "nil error", err: nil, wantOK: false},
		{name: "unrelated error", err: errors.New("mx analyze-mpr: exit 3: unknown JSON export error"), wantOK: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			name, ok := parseSelfReferenceMismatch(tc.err)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ok && name != tc.wantName {
				t.Errorf("name = %q, want %q", name, tc.wantName)
			}
		})
	}
}

func TestCopyFile(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "src.mpr")
	dst := filepath.Join(dir, "dst.mpr")
	content := []byte("hello mpr")
	if err := os.WriteFile(src, content, 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if err := copyFile(src, dst); err != nil {
		t.Fatalf("copyFile: unexpected error: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatalf("ReadFile dst: %v", err)
	}
	if string(got) != string(content) {
		t.Errorf("dst content = %q, want %q", got, content)
	}
	if _, err := os.Stat(src); err != nil {
		t.Errorf("src no longer exists: %v", err)
	}
}

func TestCopyFile_SourceMissing(t *testing.T) {
	dir := t.TempDir()
	err := copyFile(filepath.Join(dir, "does-not-exist.mpr"), filepath.Join(dir, "dst.mpr"))
	if err == nil {
		t.Fatal("copyFile returned no error for a missing source")
	}
}

func TestPrepareMpr_SucceedsWithoutRetry(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "App.mpr")
	if err := os.WriteFile(original, []byte("stub mpr content"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	bin := writeStubMx(t, "11.13.0\n", "", 0) // `mx show-version` prints a bare M.m.p line

	mprPath, result, err := PrepareMpr(context.Background(), bin, dir)
	if err != nil {
		t.Fatalf("PrepareMpr: unexpected error: %v", err)
	}
	if mprPath != original {
		t.Errorf("mprPath = %q, want %q", mprPath, original)
	}
	if result.MendixVersion != "11.13.0" {
		t.Errorf("MendixVersion = %q, want %q", result.MendixVersion, "11.13.0")
	}
}

// TestPrepareMpr_RetriesOnSelfReferenceMismatch exercises the real,
// confirmed scenario: a checkout tracked as MERA.mpr internally
// self-referring to App.mpr. The stub mx below branches on which filename
// it's asked to open — fails on MERA.mpr with the real error text, succeeds
// on App.mpr — so this test proves PrepareMpr's retry path end to end,
// including the real os-level file copy (not mocked).
func TestPrepareMpr_RetriesOnSelfReferenceMismatch(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "MERA.mpr")
	if err := os.WriteFile(original, []byte("stub mpr content"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	script := `#!/bin/sh
for arg in "$@"; do last="$arg"; done
case "$last" in
  */MERA.mpr)
    echo "Cannot open MPR file MERA.mpr... existing MPR contents refer to MPR file 'App.mpr'" >&2
    exit 1
    ;;
  */App.mpr)
    echo "11.13.0"
    exit 0
    ;;
  *)
    echo "unexpected arg: $last" >&2
    exit 99
    ;;
esac
`
	binPath := filepath.Join(t.TempDir(), "mx")
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("WriteFile stub: %v", err)
	}
	bin := Binary{Version: "stub", Path: binPath}

	mprPath, result, err := PrepareMpr(context.Background(), bin, dir)
	if err != nil {
		t.Fatalf("PrepareMpr: unexpected error: %v", err)
	}
	wantPath := filepath.Join(dir, "App.mpr")
	if mprPath != wantPath {
		t.Errorf("mprPath = %q, want %q", mprPath, wantPath)
	}
	if result.MendixVersion != "11.13.0" {
		t.Errorf("MendixVersion = %q, want %q", result.MendixVersion, "11.13.0")
	}
	if _, err := os.Stat(original); err != nil {
		t.Errorf("original MERA.mpr no longer exists — should be left untouched: %v", err)
	}
}

// TestPrepareMpr_CopyFailureKeepsOriginalError points the "wanted" rename
// target at a subdirectory that doesn't exist, so copyFile's os.Create is
// guaranteed to fail regardless of permissions or which user runs the test
// (e.g. root in a container, where a chmod-based failure wouldn't bite).
// PrepareMpr should surface the ORIGINAL mismatch error in this case, not
// the copy error — per its own logic: `return mprPath, result, err`.
func TestPrepareMpr_CopyFailureKeepsOriginalError(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "MERA.mpr")
	if err := os.WriteFile(original, []byte("stub"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	script := `#!/bin/sh
echo "existing MPR contents refer to MPR file 'nonexistent-subdir/App.mpr'" >&2
exit 1
`
	binPath := filepath.Join(t.TempDir(), "mx")
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatalf("WriteFile stub: %v", err)
	}
	bin := Binary{Version: "stub", Path: binPath}

	mprPath, _, err := PrepareMpr(context.Background(), bin, dir)
	if err == nil {
		t.Fatal("PrepareMpr returned no error despite an unresolvable rename target")
	}
	if !strings.Contains(err.Error(), "existing MPR contents refer to MPR file") {
		t.Errorf("error %q is not the original mismatch error", err.Error())
	}
	if mprPath != original {
		t.Errorf("mprPath = %q, want original %q (copy failed, should not have swapped)", mprPath, original)
	}
}