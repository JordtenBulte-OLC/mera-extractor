// internal/mx/showversion_test.go
package mx

import (
	"context"
	"strings"
	"testing"
)

func TestShowVersion_OK(t *testing.T) {
	// Exactly what a real `mx show-version <mpr>` prints: one bare M.m.p line
	// on stdout, exit 0. Captured against mx 11.12.1.
	bin := writeStubMx(t, "11.13.0\n", "", 0)

	v, err := ShowVersion(context.Background(), bin, "/whatever.mpr")
	if err != nil {
		t.Fatalf("ShowVersion: %v", err)
	}
	if v != "11.13.0" {
		t.Errorf("version = %q, want %q", v, "11.13.0")
	}
}

func TestShowVersion_StripsBOMAndTrims(t *testing.T) {
	// mx is .NET; its file/stream writers can emit a UTF-8 BOM. Also tolerate
	// stray surrounding whitespace.
	bin := writeStubMx(t, "\ufeff  11.12.1 ", "", 0)

	v, err := ShowVersion(context.Background(), bin, "/whatever.mpr")
	if err != nil {
		t.Fatalf("ShowVersion: %v", err)
	}
	if v != "11.12.1" {
		t.Errorf("version = %q, want %q", v, "11.12.1")
	}
}

func TestShowVersion_TakesFirstLineOnly(t *testing.T) {
	bin := writeStubMx(t, "11.13.0\nsome future trailing notice\n", "", 0)

	v, err := ShowVersion(context.Background(), bin, "/whatever.mpr")
	if err != nil {
		t.Fatalf("ShowVersion: %v", err)
	}
	if v != "11.13.0" {
		t.Errorf("version = %q, want %q", v, "11.13.0")
	}
}

func TestShowVersion_NonZeroExitIsError(t *testing.T) {
	// Real failure shape: show-version writes its .NET dump to STDOUT, not
	// stderr, and exits non-zero. Captured against mx 11.12.1 for a missing
	// file.
	stdout := "ERROR: System.IO.FileNotFoundException: The specified file does not exist.\n" +
		"   at Mendix.Modeler.Storage.DatabaseManager.ThrowIfNotValid(String filePath)\n"
	bin := writeStubMx(t, stdout, "", 1)

	v, err := ShowVersion(context.Background(), bin, "/missing.mpr")
	if err == nil {
		t.Fatal("expected an error for a non-zero exit")
	}
	if v != "" {
		t.Errorf("version = %q, want empty on error", v)
	}
	for _, want := range []string{"exit 1", "FileNotFoundException"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q missing %q", err.Error(), want)
		}
	}
}

func TestShowVersion_EmptyOutputOnZeroExitIsError(t *testing.T) {
	bin := writeStubMx(t, "", "", 0)

	if _, err := ShowVersion(context.Background(), bin, "/whatever.mpr"); err == nil {
		t.Fatal("expected an error when exit 0 but nothing on stdout")
	} else if !strings.Contains(err.Error(), "no version") {
		t.Errorf("error %q should explain the empty output", err.Error())
	}
}

func TestShowVersion_ExecFailureBubbles(t *testing.T) {
	bin := Binary{Version: "missing", Path: "/no/such/mx/binary"}

	if _, err := ShowVersion(context.Background(), bin, "/whatever.mpr"); err == nil {
		t.Fatal("expected a real execution failure for a missing binary")
	}
}
