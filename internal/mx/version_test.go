// internal/mx/version_test.go
package mx

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mkBinary creates <root>/<version>/modeler/mx as a stub file, mirroring
// the real /opt/mx/<version>/modeler/mx layout confirmed via the Docker
// build (see MERA-stage8-plan.md Step 1) — NOT <root>/<version>/mx.
func mkBinary(t *testing.T, root, version string) {
	t.Helper()
	dir := filepath.Join(root, version, "modeler")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	path := filepath.Join(dir, "mx")
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho stub mx\n"), 0755); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
}

func TestResolve_ExactMatch(t *testing.T) {
	root := t.TempDir()
	mkBinary(t, root, "11.13.0")

	bin, err := Resolve(root, "11.13.0")
	if err != nil {
		t.Fatalf("Resolve: unexpected error: %v", err)
	}
	wantPath := filepath.Join(root, "11.13.0", "modeler", "mx")
	if bin.Path != wantPath {
		t.Errorf("Path = %q, want %q", bin.Path, wantPath)
	}
	if bin.Version != "11.13.0" {
		t.Errorf("Version = %q, want %q", bin.Version, "11.13.0")
	}
}

func TestResolve_NoFallback(t *testing.T) {
	root := t.TempDir()
	// Only a nearby version is installed — 11.13.0 is NOT present.
	mkBinary(t, root, "11.6")

	_, err := Resolve(root, "11.13.0")
	if err == nil {
		t.Fatal("Resolve silently fell back to a nearby version — manual §1.3 requires an exact match or a loud failure")
	}
}

func TestResolve_VersionNotInstalledAtAll(t *testing.T) {
	root := t.TempDir() // empty

	_, err := Resolve(root, "11.13.0")
	if err == nil {
		t.Fatal("Resolve returned no error for a completely empty mxRoot")
	}
}

func TestResolve_PathIsDirNotBinary(t *testing.T) {
	root := t.TempDir()
	// Misconfigured install: modeler/mx exists but as a directory, not a file.
	if err := os.MkdirAll(filepath.Join(root, "11.13.0", "modeler", "mx"), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	_, err := Resolve(root, "11.13.0")
	if err == nil {
		t.Fatal("Resolve accepted a directory in place of the mx binary")
	}
}

func TestHighest_PicksNumericallyHighest(t *testing.T) {
	root := t.TempDir()
	// Deliberately includes the exact case that breaks a naive string
	// sort: "11.13.0" sorts BELOW "11.6" lexicographically, but must win
	// numerically.
	for _, v := range []string{"10.12", "10.18", "11.0", "11.6", "11.13.0"} {
		mkBinary(t, root, v)
	}

	bin, err := Highest(root)
	if err != nil {
		t.Fatalf("Highest: unexpected error: %v", err)
	}
	if bin.Version != "11.13.0" {
		t.Fatalf("Highest picked %q, want %q (likely a string-sort regression)", bin.Version, "11.13.0")
	}
	wantPath := filepath.Join(root, "11.13.0", "modeler", "mx")
	if bin.Path != wantPath {
		t.Errorf("Path = %q, want %q", bin.Path, wantPath)
	}
}

func TestHighest_SingleVersionInstalled(t *testing.T) {
	root := t.TempDir()
	mkBinary(t, root, "11.13.0")

	bin, err := Highest(root)
	if err != nil {
		t.Fatalf("Highest: unexpected error: %v", err)
	}
	if bin.Version != "11.13.0" {
		t.Errorf("Version = %q, want %q", bin.Version, "11.13.0")
	}
}

func TestHighest_SkipsNonVersionEntries(t *testing.T) {
	root := t.TempDir()
	mkBinary(t, root, "11.13.0")

	// A stray file at the root (e.g. a README) and an empty directory
	// with no modeler/mx inside it (a half-finished or corrupt install)
	// must both be ignored, not crash or get picked.
	if err := os.WriteFile(filepath.Join(root, "README.txt"), []byte("hi"), 0644); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(root, "11.99.0"), 0755); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	bin, err := Highest(root)
	if err != nil {
		t.Fatalf("Highest: unexpected error: %v", err)
	}
	if bin.Version != "11.13.0" {
		t.Errorf("Highest picked %q, want %q (should have skipped the incomplete 11.99.0 entry)", bin.Version, "11.13.0")
	}
}

func TestHighest_EmptyRoot(t *testing.T) {
	root := t.TempDir() // exists, but nothing installed

	_, err := Highest(root)
	if err == nil {
		t.Fatal("Highest returned no error for an mxRoot with no installed versions")
	}
}

func TestHighest_RootMissing(t *testing.T) {
	root := filepath.Join(t.TempDir(), "does-not-exist")

	_, err := Highest(root)
	if err == nil {
		t.Fatal("Highest returned no error for a nonexistent mxRoot")
	}
}

func TestCompareVersions(t *testing.T) {
	cases := []struct {
		a, b string
		want int
	}{
		// original a.b / a.b.c regression coverage
		{"11.13.0", "11.6", 1},
		{"11.6", "11.13.0", -1},
		{"11.6", "11.6.0", 0},
		{"10.18", "10.12", 1},
		{"11.0", "10.18", 1},
		{"11.13.0", "11.13.0", 0},

		// patch still decides before any addition is even looked at
		{"11.13.1", "11.13.0.zzz", 1},

		// same a.b.c, addition present on both — plain string compare
		{"11.13.0.beta-1", "11.13.0.rc-1", -1}, // 'b' < 'r'
		{"11.13.0.68410", "11.13.0.68411", -1}, // string compare, not numeric —
		// "68410" < "68411" here still holds under string compare, but this
		// would NOT generalize to e.g. "9" vs "10" (string "10" < "9").
		// If Mendix's addition field turns out to need numeric comparison
		// for a real case like that, this is the function to revisit.

		// same a.b.c, addition present on only one side — absence ranks
		// lower under the assumption documented on compareVersions.
		{"11.13.0", "11.13.0.68410", -1},
		{"11.13.0.68410", "11.13.0", 1},
	}
	for _, tc := range cases {
		got := compareVersions(tc.a, tc.b)
		norm := 0
		if got < 0 {
			norm = -1
		} else if got > 0 {
			norm = 1
		}
		if norm != tc.want {
			t.Errorf("compareVersions(%q, %q) = %d, want sign %d", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestParseVersion(t *testing.T) {
	cases := []struct {
		in   string
		want parsedVersion
	}{
		{"11.6", parsedVersion{major: 11, minor: 6}},
		{"11.13.0", parsedVersion{major: 11, minor: 13, patch: 0}},
		{"11.13.0.68410", parsedVersion{major: 11, minor: 13, patch: 0, addition: "68410"}},
		{"11.13.0.beta-1", parsedVersion{major: 11, minor: 13, patch: 0, addition: "beta-1"}},
		// addition containing its own dot must stay intact, not get cut
		// at the first internal separator.
		{"11.13.0.68410-beta.2", parsedVersion{major: 11, minor: 13, patch: 0, addition: "68410-beta.2"}},
	}
	for _, tc := range cases {
		got := parseVersion(tc.in)
		if got != tc.want {
			t.Errorf("parseVersion(%q) = %+v, want %+v", tc.in, got, tc.want)
		}
	}
}

func TestParseVersion_NeverPanics(t *testing.T) {
	cases := []string{
		"",
		".",
		"...",
		"11",
		"11.",
		".11",
		"11.13.0.",
		"11.13.0.beta-1.extra.stuff",
		"abc.def",
		"11.13.0.日本語",
		"11.13.0.-",
		strings.Repeat("1.", 1000) + "0", // pathologically long
	}
	for _, c := range cases {
		func() {
			defer func() {
				if r := recover(); r != nil {
					t.Errorf("parseVersion(%q) panicked: %v", c, r)
				}
			}()
			_ = parseVersion(c)
		}()
	}
}

// FuzzCompareVersions backs up "must not break" with something stronger
// than a fixed case list: run the comparator against inputs no one wrote
// by hand. It also checks a basic sanity property — swapping the two
// arguments should flip the sign, never keep the same nonzero sign — since
// that's cheap to check and would catch a broken comparator even when
// nothing panics.
func FuzzCompareVersions(f *testing.F) {
	seeds := []string{
		"11.13.0", "11.6", "11.13.0.68410", "11.13.0.beta-1",
		"", "abc", "11.13.0.日本語", "10.12", "11.0",
	}
	for _, a := range seeds {
		for _, b := range seeds {
			f.Add(a, b)
		}
	}
	f.Fuzz(func(t *testing.T, a, b string) {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("compareVersions(%q, %q) panicked: %v", a, b, r)
			}
		}()
		got := compareVersions(a, b)
		rev := compareVersions(b, a)
		if (got < 0 && rev < 0) || (got > 0 && rev > 0) {
			t.Fatalf("not antisymmetric: compareVersions(%q,%q)=%d but compareVersions(%q,%q)=%d",
				a, b, got, b, a, rev)
		}
	})
}
