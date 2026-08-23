// internal/mx/listversions_test.go
package mx

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

// mxRootWith builds a fake MERA_MX_ROOT. A name in `withBinary` gets a real
// <name>/modeler/mx file; a name in `dirsOnly` gets the directory but no
// binary, standing in for a half-extracted or half-trimmed install.
func mxRootWith(t *testing.T, withBinary []string, dirsOnly []string) string {
	t.Helper()
	root := t.TempDir()
	for _, name := range withBinary {
		dir := filepath.Join(root, name, "modeler")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "mx"), []byte("#!/bin/sh\n"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, name := range dirsOnly {
		if err := os.MkdirAll(filepath.Join(root, name, "modeler"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

func versionsOf(bins []Binary) []string { return versionStrings(bins) }

func TestListVersions_SortsNumericallyNotAsStrings(t *testing.T) {
	// The case string sorting gets wrong: "11.9.0" > "11.13.0" alphabetically.
	root := mxRootWith(t, []string{"11.13.0", "11.9.0", "10.24.1", "11.6"}, nil)

	got, err := ListVersions(root)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"10.24.1", "11.6", "11.9.0", "11.13.0"}
	if !reflect.DeepEqual(versionsOf(got), want) {
		t.Errorf("ListVersions = %v, want %v", versionsOf(got), want)
	}

	// Highest must agree with the tail of that list.
	h, err := Highest(root)
	if err != nil {
		t.Fatal(err)
	}
	if h.Version != "11.13.0" {
		t.Errorf("Highest = %q, want 11.13.0", h.Version)
	}
}

// The concrete reason isVersionLike exists: .mx-binaries still contains a
// `tmp/` staging folder from early manual work. Without a name check it would
// be advertised as a supported Mendix version, and parseVersion would sort it
// as 0.0.0 because it discards Atoi errors.
func TestListVersions_IgnoresNonVersionDirectories(t *testing.T) {
	root := mxRootWith(t, []string{"11.13.0", "tmp", "scratch", "beta.1", "11"}, nil)

	got, err := ListVersions(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"11.13.0"}; !reflect.DeepEqual(versionsOf(got), want) {
		t.Errorf("ListVersions = %v, want %v", versionsOf(got), want)
	}
}

func TestListVersions_IgnoresIncompleteInstalls(t *testing.T) {
	// A version directory with no modeler/mx is a failed extract or a failed
	// trim, not a usable version.
	root := mxRootWith(t, []string{"11.13.0"}, []string{"11.12.0"})

	got, err := ListVersions(root)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"11.13.0"}; !reflect.DeepEqual(versionsOf(got), want) {
		t.Errorf("ListVersions = %v, want %v", versionsOf(got), want)
	}
	// And Resolve must refuse it, in step.
	if _, err := Resolve(root, "11.12.0"); err == nil {
		t.Error("Resolve accepted a version with no binary")
	}
}

// Unreadable and empty are different faults with different fixes, so they must
// not collapse into the same result.
func TestListVersions_EmptyIsNotAnError(t *testing.T) {
	got, err := ListVersions(t.TempDir())
	if err != nil {
		t.Fatalf("a readable but empty root is not an error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %v, want none", versionsOf(got))
	}
}

func TestListVersions_UnreadableRootIsAnError(t *testing.T) {
	_, err := ListVersions(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("a missing root must be an error")
	}
	if !strings.Contains(err.Error(), "does-not-exist") {
		t.Errorf("error should name the path: %v", err)
	}
}

func TestListVersions_RelativeRootErrorNamesResolvedPath(t *testing.T) {
	// Same lesson as describeMxRoot in internal/api: a relative root resolves
	// against a working directory the reader cannot see.
	_, err := ListVersions("./definitely-not-here")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "resolved to") {
		t.Errorf("error should show where that actually points: %v", err)
	}
}

// The property that motivated the refactor: /health, Resolve and Highest all
// read the same list, so none can disagree about what is supported.
func TestResolveAndHighestAgreeWithListVersions(t *testing.T) {
	root := mxRootWith(t, []string{"11.13.0", "11.12.0"}, []string{"tmp", "11.11.0"})

	installed, err := ListVersions(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, b := range installed {
		got, err := Resolve(root, b.Version)
		if err != nil {
			t.Errorf("ListVersions offered %q but Resolve refused it: %v", b.Version, err)
			continue
		}
		if got.Path != b.Path {
			t.Errorf("Resolve(%q).Path = %q, ListVersions said %q", b.Version, got.Path, b.Path)
		}
	}
	for _, absent := range []string{"tmp", "11.11.0", "9.0.0"} {
		if _, err := Resolve(root, absent); err == nil {
			t.Errorf("Resolve accepted %q, which ListVersions does not offer", absent)
		}
	}
	h, err := Highest(root)
	if err != nil {
		t.Fatal(err)
	}
	if h != installed[len(installed)-1] {
		t.Errorf("Highest = %+v, want the last of ListVersions %+v", h, installed[len(installed)-1])
	}
}

// "no binary for this version" and "no binaries at all" need different fixes
// and used to produce the same message.
func TestResolve_ErrorNamesWhatIsInstalled(t *testing.T) {
	root := mxRootWith(t, []string{"11.13.0", "11.12.0"}, nil)
	_, err := Resolve(root, "10.6.0")
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, want := range []string{"10.6.0", "11.12.0", "11.13.0"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should mention %q: %v", want, err)
		}
	}

	_, err = Resolve(t.TempDir(), "10.6.0")
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "no versions installed") {
		t.Errorf("an empty root should say so plainly: %v", err)
	}
}

func TestIsVersionLike(t *testing.T) {
	cases := map[string]bool{
		"11.13.0":     true,
		"11.6":        true,
		"10.24.1.500": true,
		// Only the first two segments are checked, so a suffix on the third
		// rides along. Accepting it is fine: the modeler/mx path check still
		// gates usability, and parseVersion handles the tail.
		"11.13.0-beta": true,
		"tmp":          false,
		"scratch":      false,
		"11":           false,
		"beta.1":       false,
		"":             false,
		".":            false,
		"-1.0":         false,
		"1.-1":         false,
		"v11.13":       false,
	}
	for name, want := range cases {
		if got := isVersionLike(name); got != want {
			t.Errorf("isVersionLike(%q) = %v, want %v", name, got, want)
		}
	}
}
