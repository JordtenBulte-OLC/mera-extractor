// internal/mx/analyze_test.go
package mx

import (
	"context"
	"strings"
	"testing"
)

func TestParseAnalyzeOutput_VersionParsing(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		want   string
	}{
		{"plain version line", "Some banner\nMendix version: 11.13.0\nOther stuff\n", "11.13.0"},
		{"version line with extra whitespace", "Mendix version:    11.6   \n", "11.6"},
		{"no version line present", "nothing relevant here\n", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := parseAnalyzeOutput(tc.stdout)
			if res.MendixVersion != tc.want {
				t.Errorf("MendixVersion = %q, want %q", res.MendixVersion, tc.want)
			}
		})
	}
}

func TestParseAnalyzeOutput_ProjectConversionDetection(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		want   bool
	}{
		{"conversion unit present", "Type breakdown:\nProjects$ProjectConversion: 1\nMicroflows$Microflow: 42\n", true},
		{"no conversion unit", "Type breakdown:\nMicroflows$Microflow: 42\n", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := parseAnalyzeOutput(tc.stdout)
			if res.HasProjectConversion != tc.want {
				t.Errorf("HasProjectConversion = %v, want %v", res.HasProjectConversion, tc.want)
			}
		})
	}
}

func TestParseAnalyzeOutput_RawPreserved(t *testing.T) {
	stdout := "whatever mx printed, verbatim\n"
	res := parseAnalyzeOutput(stdout)
	if res.Raw != stdout {
		t.Errorf("Raw = %q, want %q", res.Raw, stdout)
	}
}

func TestAnalyze_Success(t *testing.T) {
	bin := writeStubMx(t, "Mendix version: 11.13.0\nNo conversion here\n", "", 0)

	res, err := Analyze(context.Background(), bin, "/fake/App.mpr")
	if err != nil {
		t.Fatalf("Analyze: unexpected error: %v", err)
	}
	if res.MendixVersion != "11.13.0" {
		t.Errorf("MendixVersion = %q, want %q", res.MendixVersion, "11.13.0")
	}
	if res.HasProjectConversion {
		t.Error("HasProjectConversion = true, want false")
	}
}

func TestAnalyze_ProjectConversionDetected(t *testing.T) {
	bin := writeStubMx(t, "Mendix version: 11.10.0\nProjects$ProjectConversion: 1\n", "", 0)

	res, err := Analyze(context.Background(), bin, "/fake/App.mpr")
	if err != nil {
		t.Fatalf("Analyze: unexpected error: %v", err)
	}
	if !res.HasProjectConversion {
		t.Error("HasProjectConversion = false, want true — this is the migration-commit trap (#16)")
	}
}

func TestAnalyze_NonZeroExitIsError(t *testing.T) {
	bin := writeStubMx(t, "", "boom: something went wrong", 3)

	_, err := Analyze(context.Background(), bin, "/fake/App.mpr")
	if err == nil {
		t.Fatal("Analyze returned no error for a non-zero exit")
	}
	if !strings.Contains(err.Error(), "boom: something went wrong") {
		t.Errorf("error %q does not include stderr", err.Error())
	}
}

func TestAnalyze_RealExecutionFailure(t *testing.T) {
	bin := Binary{Version: "missing", Path: "/definitely/does/not/exist/mx"}

	_, err := Analyze(context.Background(), bin, "/fake/App.mpr")
	if err == nil {
		t.Fatal("Analyze returned no error for a nonexistent binary")
	}
}