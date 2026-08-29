// internal/mx/analyze.go
package mx

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

type AnalyzeResult struct {
	MendixVersion string

	// UnitTypeCounts is the "Size by unit type" table: metamodel type string →
	// number of units of that type in this snapshot. This is the app's real
	// unit-type inventory, straight from the tool.
	//
	// CAUTION: these are analyze-mpr's STORAGE-level type names, which are NOT
	// always the names `mx dump-mpr` reports for the same unit. Confirmed
	// against one real app: analyze-mpr says Forms$Page / Forms$Snippet /
	// Forms$Layout / Forms$BuildingBlock where dump-mpr says Pages$Page /
	// Pages$Snippet. Other types (Microflows$Microflow, Images$ImageCollection,
	// JavaActions$JavaAction, DomainModels$DomainModel, Projects$Folder…) are
	// spelled identically in both. Do not treat this map as authoritative for
	// anything that has to match dump-mpr or diff output.
	UnitTypeCounts map[string]int

	// HasProjectConversion reports that a Projects$ProjectConversion unit is
	// present.
	//
	// This is INFORMATIONAL ONLY. It does not mean the snapshot is mid
	// Studio-Pro-version migration and does not mean mx cannot parse it — see
	// ErrUnsupportedVersionMigrationCommit for why that inference was wrong.
	HasProjectConversion bool

	Raw string
}

// ErrUnsupportedVersionMigrationCommit indicates mx could not parse an .mpr
// snapshot because it captures a Studio Pro version upgrade in progress. It
// surfaces as `Expected '$ID' as the first property of a storage object, but
// got 'Associations'` — a parse exception that looks exactly like an mx bug
// and is not (manual trap #16).
//
// DETECTED ON FAILURE, NOT PREDICTED. An earlier implementation returned this
// whenever `mx analyze-mpr` mentioned Projects$ProjectConversion anywhere in
// its output, and that was wrong twice over:
//
//   - A Projects$ProjectConversion unit PERSISTS in the model after an upgrade
//     completes — it is a record of the conversion, not a conversion in
//     progress. A real, healthy app carries exactly one and diffs perfectly.
//   - analyze-mpr's output is a size report. The unit merely appearing in the
//     "Size by unit type" table says nothing about parseability.
//
// Manual trap #16 actually says "IF DIFFING FAILS on a project containing a
// Projects$ProjectConversion unit, treat that as known-unsupported". The
// condition is the failure; the unit is only the explanation. Reading it as an
// unconditional pre-check rejected every commit of any app that had ever been
// upgraded.
type ErrUnsupportedVersionMigrationCommit struct {
	MendixVersion string
}

func (e *ErrUnsupportedVersionMigrationCommit) Error() string {
	return fmt.Sprintf("mx: .mpr is mid version-migration (Mendix version %s) — not supported", e.MendixVersion)
}

// VersionMigrationSignature is the fragment of mx's parse exception that
// identifies an unreadable mid-migration snapshot. Callers match it against a
// failed diff or dump-mpr's stderr — that is the authoritative signal.
//
// Deliberately short: the full message names a property ('Associations' in the
// one real occurrence) that is unlikely to be stable across snapshots.
const VersionMigrationSignature = "as the first property of a storage object"

// IsVersionMigrationFailure reports whether a subprocess's stderr carries that
// parse exception.
func IsVersionMigrationFailure(stderr string) bool {
	return strings.Contains(stderr, VersionMigrationSignature)
}

// Analyze runs `mx analyze-mpr` and extracts the Mendix version plus the
// snapshot's unit-type inventory.
//
// NOT CURRENTLY WIRED IN. PrepareMpr moved to ShowVersion after analyze-mpr
// was found to abort the whole process with an unbounded-recursion stack
// overflow in MprStats on some real models (SIGABRT, exit 134 — see
// MERA-session-status.md). This function and its parser are kept intact,
// with their tests, because analyze-mpr's UnitTypeCounts inventory is the
// only source for that data and is likely wanted again once Mendix fixes the
// crash. Re-wire it in PrepareMpr behind an abort-detecting fallback to
// ShowVersion if you bring it back.
//
// analyze-mpr is confirmed version-agnostic — a single mx build correctly read
// an 11.10.0-authored file during this project's own $ID/Associations
// investigation — so this is meant to be called with whatever Highest()
// returns, BEFORE the real version-matched binary is even known. That resolves
// the chicken-and-egg problem of "need the version to pick a binary, need a
// binary to read the version."
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

// analyzeSections are the headers analyze-mpr prints. Only one is parsed, but
// knowing them all is what lets the parser tell "Size by unit type" apart from
// the tables that follow it — "Size by property" rows look almost identical
// and would otherwise pollute UnitTypeCounts with entries like
// "Forms$PageTemplate.ImageData".
var analyzeSections = map[string]bool{
	"MPR File Analysis":  true,
	"BSON contents":      true,
	"Content categories": true,
	"Size by unit type":  true,
	"Size by property":   true,
	"Size by unit":       true,
	"Size by module":     true,
}

// parseAnalyzeOutput reads analyze-mpr's plain-text report.
//
// Deliberately tolerant — this is free-form CLI text from an undocumented
// tool, not a stable format — so a missing section degrades to a zero value
// rather than an error.
func parseAnalyzeOutput(stdout string) AnalyzeResult {
	stdout = strings.TrimPrefix(stdout, "\ufeff")
	res := AnalyzeResult{Raw: stdout, UnitTypeCounts: map[string]int{}}

	section := ""
	for _, line := range strings.Split(stdout, "\n") {
		trimmed := strings.TrimSpace(line)

		// A blank line ends the current section; each header is preceded by one.
		if trimmed == "" {
			section = ""
			continue
		}
		if analyzeSections[trimmed] {
			section = trimmed
			continue
		}
		if strings.HasPrefix(trimmed, "---") {
			continue // the rule under each header
		}

		if v, ok := strings.CutPrefix(trimmed, "Mendix version:"); ok {
			res.MendixVersion = strings.TrimSpace(v)
			continue
		}

		if section == "Size by unit type" {
			if name, count, ok := parseUnitTypeRow(trimmed); ok {
				res.UnitTypeCounts[name] = count
			}
		}
	}

	res.HasProjectConversion = res.UnitTypeCounts["Projects$ProjectConversion"] > 0
	return res
}

// parseUnitTypeRow reads one row of the "Size by unit type" table:
//
//	Microflows$Microflow |    2,752,404 bytes | 1,816.14% of MPR |    242 occurrences
func parseUnitTypeRow(line string) (name string, count int, ok bool) {
	fields := strings.Split(line, "|")
	if len(fields) < 4 {
		return "", 0, false
	}
	name = strings.TrimSpace(fields[0])
	// Unit types are Namespace$Type. A dot means this is a PROPERTY row
	// (Forms$PageTemplate.ImageData) that leaked in from an adjacent section.
	if !strings.Contains(name, "$") || strings.Contains(name, ".") {
		return "", 0, false
	}
	occ := strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(fields[len(fields)-1]), "occurrences"))
	n, err := strconv.Atoi(strings.ReplaceAll(occ, ",", ""))
	if err != nil {
		return "", 0, false
	}
	return name, n, true
}
