// internal/mx/analyze_parse_test.go
package mx

import (
	"os"
	"strings"
	"testing"
)

func realAnalyzeOutput(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("testdata/analyze-mera-head.txt")
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestParseAnalyzeOutput_RealCapturedReport(t *testing.T) {
	res := parseAnalyzeOutput(realAnalyzeOutput(t))

	if res.MendixVersion != "11.13.0" {
		t.Errorf("MendixVersion = %q, want 11.13.0", res.MendixVersion)
	}

	// Spot-check the full "Size by unit type" table, including the last row —
	// if the section terminator were wrong, the tail would be missing.
	want := map[string]int{
		"Forms$Page":                         55,
		"Forms$PageTemplate":                 47,
		"Images$ImageCollection":             10,
		"Forms$BuildingBlock":                55,
		"Microflows$Microflow":               242,
		"Forms$Snippet":                      18,
		"JavaScriptActions$JavaScriptAction": 72,
		"JavaActions$JavaAction":             141,
		"DomainModels$DomainModel":           16,
		"Forms$Layout":                       25,
		"CustomIcons$CustomIconCollection":   3,
		"Microflows$Nanoflow":                16,
		"JsonStructures$JsonStructure":       6,
		"Texts$SystemTextCollection":         1,
		"Enumerations$Enumeration":           35,
		"ImportMappings$ImportMapping":       6,
		"ExportMappings$ExportMapping":       4,
		"Projects$Folder":                    200,
		"Projects$ProjectConversion":         1,
		"Projects$ModuleSettings":            16,
	}
	for name, count := range want {
		if got := res.UnitTypeCounts[name]; got != count {
			t.Errorf("UnitTypeCounts[%q] = %d, want %d", name, got, count)
		}
	}
	if len(res.UnitTypeCounts) != len(want) {
		t.Errorf("parsed %d unit types, want exactly %d: %v",
			len(res.UnitTypeCounts), len(want), res.UnitTypeCounts)
	}
}

// The "Size by property" table immediately follows "Size by unit type" and its
// rows are shaped identically. Without section tracking plus the dot check,
// entries like Forms$PageTemplate.ImageData land in the unit-type inventory.
func TestParseAnalyzeOutput_IgnoresAdjacentTables(t *testing.T) {
	res := parseAnalyzeOutput(realAnalyzeOutput(t))
	for name := range res.UnitTypeCounts {
		if strings.Contains(name, ".") {
			t.Errorf("property row %q leaked into UnitTypeCounts", name)
		}
	}
	for _, leaked := range []string{
		"Images$Image.Image",                            // Size by property
		"CodeActions$MicroflowActionInfo.ImageDataDark", // Size by property
		"[Marketplace] Atlas_Web_Content",               // Size by module
		"IDs (model data)",                              // BSON contents
	} {
		if _, ok := res.UnitTypeCounts[leaked]; ok {
			t.Errorf("%q should not be in UnitTypeCounts", leaked)
		}
	}
}

// The whole point of the rewrite: a healthy app that has been upgraded at some
// point carries a Projects$ProjectConversion unit forever. Reporting it is
// fine; concluding "unparseable" from it is not.
func TestParseAnalyzeOutput_ProjectConversionIsInformationalOnly(t *testing.T) {
	res := parseAnalyzeOutput(realAnalyzeOutput(t))
	if !res.HasProjectConversion {
		t.Fatal("this fixture does contain one Projects$ProjectConversion unit")
	}
	if res.UnitTypeCounts["Projects$ProjectConversion"] != 1 {
		t.Errorf("count = %d, want 1", res.UnitTypeCounts["Projects$ProjectConversion"])
	}
	// The version still parses, and the report is otherwise complete — this
	// snapshot is perfectly readable.
	if res.MendixVersion == "" || len(res.UnitTypeCounts) < 10 {
		t.Error("a snapshot with a ProjectConversion unit still analyses normally")
	}
}

func TestIsVersionMigrationFailure(t *testing.T) {
	real := "Expected '$ID' as the first property of a storage object, but got 'Associations'."
	if !IsVersionMigrationFailure(real) {
		t.Error("the real parse exception must match")
	}
	// The property name varies; the signature must not depend on it.
	if !IsVersionMigrationFailure("Expected '$ID' as the first property of a storage object, but got 'Whatever'") {
		t.Error("signature should be independent of the offending property name")
	}
	for _, other := range []string{
		"",
		"mx diff: some unrelated failure",
		"existing MPR contents refer to MPR file 'App.mpr'", // the OTHER named failure mode
	} {
		if IsVersionMigrationFailure(other) {
			t.Errorf("%q should not match", other)
		}
	}
}

func TestParseAnalyzeOutput_Degrades(t *testing.T) {
	res := parseAnalyzeOutput("")
	if res.MendixVersion != "" || len(res.UnitTypeCounts) != 0 || res.HasProjectConversion {
		t.Errorf("empty input should give a zero result, got %+v", res)
	}
	// A BOM is confirmed present on this tool family's file output; harmless
	// to guard on stdout too.
	res = parseAnalyzeOutput("\ufeff      Mendix version: 10.6.1\n")
	if res.MendixVersion != "10.6.1" {
		t.Errorf("MendixVersion = %q with a BOM prefix", res.MendixVersion)
	}
}
