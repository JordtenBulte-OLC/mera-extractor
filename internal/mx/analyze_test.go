// internal/mx/analyze_test.go
package mx

import (
	"context"
	"fmt"
	"strings"
	"testing"
)

// analyzeReport builds a report in analyze-mpr's REAL layout.
//
// The original tests here invented a "Projects$ProjectConversion: 1" line,
// which the tool never emits — the actual format is a pipe-delimited table
// under a "Size by unit type" header. Those tests passed against a parser that
// merely scanned for the type name anywhere in the output, and that looseness
// is what made a healthy app look like a broken one in production.
//
// The blank line before each header is load-bearing: it is how the parser
// closes the preceding section.
func analyzeReport(version string, unitTypeRows ...string) string {
	rule := strings.Repeat("-", 76)
	var b strings.Builder
	fmt.Fprintf(&b, "\nMPR File Analysis\n%s\n", rule)
	b.WriteString("            MPR File: /fake/App.mpr\n")
	b.WriteString("        Size on disk: 151,552 bytes\n")
	if version != "" {
		fmt.Fprintf(&b, "      Mendix version: %s\n", version)
	}
	b.WriteString("     Number of units: 1,037\n")
	fmt.Fprintf(&b, "\nSize by unit type\n%s\n", rule)
	for _, row := range unitTypeRows {
		fmt.Fprintf(&b, "%s\n", row)
	}
	return b.String()
}

// unitTypeRow renders one table row the way the tool spaces it. occurrences is
// a string so a test can pass a comma-formatted count verbatim — every real
// count over 999 arrives as "1,037".
func unitTypeRow(typeName, occurrences string) string {
	return fmt.Sprintf("    %s |    123,456 bytes | 81.46%% of MPR |    %s occurrences",
		typeName, occurrences)
}

func TestParseAnalyzeOutput_VersionParsing(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		want   string
	}{
		{"plain version line", "Some banner\nMendix version: 11.13.0\nOther stuff\n", "11.13.0"},
		{"version line with extra whitespace", "Mendix version:    11.6   \n", "11.6"},
		{"no version line present", "nothing relevant here\n", ""},
		{"inside a real report", analyzeReport("11.13.0"), "11.13.0"},
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

// HasProjectConversion now means "the unit-type inventory contains at least one
// of these", NOT "this snapshot is mid-migration". That inference was wrong:
// the unit persists in the model after an upgrade completes, so a healthy app
// carries one forever. Unparseability is detected from a failed diff instead —
// see IsVersionMigrationFailure.
func TestParseAnalyzeOutput_ProjectConversionDetection(t *testing.T) {
	cases := []struct {
		name   string
		stdout string
		want   bool
	}{
		{
			"conversion unit present in the table",
			analyzeReport("11.13.0",
				unitTypeRow("Microflows$Microflow", "242"),
				unitTypeRow("Projects$ProjectConversion", "1")),
			true,
		},
		{
			"no conversion unit",
			analyzeReport("11.13.0", unitTypeRow("Microflows$Microflow", "242")),
			false,
		},
		{
			// The regression that started this: a bare mention outside the
			// unit-type table must NOT set the flag. The old substring scan
			// matched anywhere, including prose, another table, or a filename.
			"bare mention outside the table",
			"Some banner mentioning Projects$ProjectConversion in passing\n" +
				analyzeReport("11.13.0", unitTypeRow("Microflows$Microflow", "242")),
			false,
		},
		{
			// A property row in "Size by property" is shaped like a unit-type
			// row and sits directly below it.
			"property row is not a unit row",
			analyzeReport("11.13.0", unitTypeRow("Microflows$Microflow", "242")) +
				"\nSize by property\n" + strings.Repeat("-", 76) + "\n" +
				unitTypeRow("Projects$ProjectConversion.Something", "5") + "\n",
			false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := parseAnalyzeOutput(tc.stdout)
			if res.HasProjectConversion != tc.want {
				t.Errorf("HasProjectConversion = %v, want %v\nUnitTypeCounts=%v",
					res.HasProjectConversion, tc.want, res.UnitTypeCounts)
			}
		})
	}
}

func TestParseAnalyzeOutput_UnitTypeCounts(t *testing.T) {
	res := parseAnalyzeOutput(analyzeReport("11.13.0",
		unitTypeRow("Microflows$Microflow", "242"),
		unitTypeRow("Forms$Page", "55"),
		unitTypeRow("Projects$Folder", "1,037"),
		unitTypeRow("Texts$SystemTextCollection", "1"),
	))

	want := map[string]int{
		"Microflows$Microflow":       242,
		"Forms$Page":                 55,
		"Projects$Folder":            1037, // comma-formatted in the report
		"Texts$SystemTextCollection": 1,
	}
	if len(res.UnitTypeCounts) != len(want) {
		t.Fatalf("parsed %d types, want %d: %v", len(res.UnitTypeCounts), len(want), res.UnitTypeCounts)
	}
	for name, count := range want {
		if got := res.UnitTypeCounts[name]; got != count {
			t.Errorf("UnitTypeCounts[%q] = %d, want %d", name, got, count)
		}
	}
	// Thousands separators must survive the round trip.
	if res.UnitTypeCounts["Projects$Folder"] != 1037 {
		t.Errorf("comma-formatted count parsed as %d, want 1037", res.UnitTypeCounts["Projects$Folder"])
	}
}

func TestParseAnalyzeOutput_UnitTypeCountsIsNeverNil(t *testing.T) {
	// Callers index it directly; a nil map would read fine but a later write
	// would panic, and the zero-value case is the one nobody tests by hand.
	if res := parseAnalyzeOutput(""); res.UnitTypeCounts == nil {
		t.Error("UnitTypeCounts should be an empty map, not nil")
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
	bin := writeStubMx(t, analyzeReport("11.13.0", unitTypeRow("Microflows$Microflow", "42")), "", 0)

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
	if res.UnitTypeCounts["Microflows$Microflow"] != 42 {
		t.Errorf("UnitTypeCounts = %v", res.UnitTypeCounts)
	}
}

// Renamed from TestAnalyze_ProjectConversionDetected: the flag reports an
// inventory fact, and no longer implies the migration-commit trap (#16).
func TestAnalyze_ProjectConversionReported(t *testing.T) {
	bin := writeStubMx(t, analyzeReport("11.10.0",
		unitTypeRow("Microflows$Microflow", "42"),
		unitTypeRow("Projects$ProjectConversion", "1")), "", 0)

	res, err := Analyze(context.Background(), bin, "/fake/App.mpr")
	if err != nil {
		t.Fatalf("Analyze: unexpected error: %v", err)
	}
	if !res.HasProjectConversion {
		t.Error("HasProjectConversion = false, want true")
	}
	// It must NOT be treated as a failure — this snapshot analyses fine, and
	// callers are expected to proceed with a warning.
	if res.MendixVersion != "11.10.0" {
		t.Errorf("the rest of the report must still parse; MendixVersion = %q", res.MendixVersion)
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
