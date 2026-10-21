// internal/mx/resolve.go
package mx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// ResolvedUnit is what a diff unit's bare $ID resolves to via dump-mpr.
//
// QualifiedNameSynthesized is false for the common case — dump-mpr gave the
// unit a real $QualifiedName directly (pages, microflows, entities,
// attributes, associations, enumeration values, module roles, and other
// named documents/members all do). It is true when dump-mpr gave the unit
// $ID but no $QualifiedName at all — confirmed against a real dump-mpr
// export: Projects$Folder, DomainModels$DomainModel, Projects$ModuleSettings
// and Security$ModuleSecurity are all like this, and any other
// container-only unit type would be too. For those, QualifiedName is
// synthesized by walking the $ContainerID chain up to the nearest ancestor
// that does have one (see synthesizeQualifiedName) — callers that care about
// the distinction (e.g. to render "(inferred)") can check this flag; callers
// that don't can ignore it and just use QualifiedName either way.
type ResolvedUnit struct {
	ID                       string
	Type                     string
	QualifiedName            string
	Module                   string // QualifiedName's prefix before the first "."
	QualifiedNameSynthesized bool
}

// DumpMprExitError carries dump-mpr's own exit code (0-4) and its meaning
// — a DIFFERENT table from mx diff's, deliberately not reusing diff.go's
// error types even though some numeric codes overlap.
type DumpMprExitError struct {
	ExitCode int
	Stderr   string
}

func (e *DumpMprExitError) Error() string {
	meaning := map[int]string{
		1: "wrong project file provided",
		2: "invalid unit type(s)",
		3: "unknown JSON export error",
		4: "project is in a different Mendix version",
	}[e.ExitCode]
	if meaning == "" {
		meaning = "unexpected exit code"
	}
	return fmt.Sprintf("mx dump-mpr: exit %d (%s): %s", e.ExitCode, meaning, e.Stderr)
}

// maxContainerChainDepth bounds synthesizeQualifiedName's upward walk. A
// real Mendix containment chain is never anywhere near this deep (module ->
// folder -> a few nested folders at most) — this exists purely to turn a
// malformed or cyclic $ContainerID chain into a clean "can't resolve"
// instead of an infinite loop.
const maxContainerChainDepth = 32

// ResolveQualifiedNames runs ONE `mx dump-mpr` call against mprPath,
// filtered via a single comma-joined --unit-type value (confirmed: dump-mpr
// accepts 'Type1,Type2,...' in one invocation), and returns a map from id to
// ResolvedUnit for every id in wantIDs that was found. "Projects$Module" is
// always added to the requested unit types (see ensureModuleType) so that
// every unit's containment chain has a resolvable base case.
//
// Decodes generically (json.Unmarshal into any) and indexes every object
// carrying $ID by that id — deliberately not modeling dump-mpr's exact
// nesting schema (undocumented), and deliberately not assuming
// $QualifiedName is only ever found on top-level documents: confirmed
// against real dump-mpr output that plenty of nested objects carry it too
// (DomainModels$Attribute, DomainModels$Association/CrossAssociation,
// Enumerations$EnumerationValue, Pages$PageParameter/SnippetParameter,
// JavaActions$JavaActionParameter, Microflows$MicroflowParameterObject,
// Security$ModuleRole, at minimum) — those just resolve directly with no
// synthesis needed, same as a top-level document.
//
// unitTypes may be spelled in either vocabulary — mx diff's storage-level
// Forms$ names are translated (see dumpMprUnitTypes). If dump-mpr still
// rejects some of them (exit 2), they are dropped and the call is retried
// once without them: the result then comes back non-nil TOGETHER with an
// *UnitTypesSkippedError naming what was dropped. Any other error comes back
// with a nil map.
func ResolveQualifiedNames(ctx context.Context, bin Binary, mprPath string, unitTypes []string, wantIDs map[string]bool) (map[string]ResolvedUnit, error) {
	types := ensureModuleType(dumpMprUnitTypes(unitTypes))

	found, err := dumpAndResolve(ctx, bin, mprPath, types, wantIDs)
	var exitErr *DumpMprExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode != 2 {
		return found, err
	}

	// ▶ Why retry rather than fail: the --unit-type filter only narrows the
	// dump, resolution matches on $ID alone. One type string dump-mpr does
	// not know must cost the units of that type their names, not every unit
	// in the diff — which is what a single Forms$Page did until 2026-10-22.
	invalid := parseInvalidUnitTypes(exitErr.Stderr)
	kept := withoutTypes(types, invalid)
	if len(invalid) == 0 || len(kept) == len(types) {
		return nil, err // nothing recognisable to drop; retrying would fail the same way
	}
	found, err = dumpAndResolve(ctx, bin, mprPath, kept, wantIDs)
	if err != nil {
		return nil, err
	}
	return found, &UnitTypesSkippedError{Types: invalid}
}

// UnitTypesSkippedError reports a partial success: dump-mpr rejected Types,
// the call was retried without them, and units of those types are simply
// absent from the result.
type UnitTypesSkippedError struct {
	Types []string
}

func (e *UnitTypesSkippedError) Error() string {
	return fmt.Sprintf("mx dump-mpr does not know unit type(s) %s; units of those types are unnamed",
		strings.Join(e.Types, ", "))
}

func dumpAndResolve(ctx context.Context, bin Binary, mprPath string, types []string, wantIDs map[string]bool) (map[string]ResolvedUnit, error) {
	outPath := filepath.Join(os.TempDir(), fmt.Sprintf("mera-dump-%d.json", time.Now().UnixNano()))
	defer os.Remove(outPath)

	res, err := run(ctx, bin, 5*time.Minute, "dump-mpr", mprPath,
		"--unit-type", strings.Join(types, ","),
		"--output-file", outPath)
	if err != nil {
		return nil, err // real execution failure
	}
	if res.ExitCode != 0 {
		return nil, &DumpMprExitError{ExitCode: res.ExitCode, Stderr: res.Stderr}
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		return nil, fmt.Errorf("mx dump-mpr: reading %s: %w", outPath, err)
	}
	data = stripBOM(data)
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("mx dump-mpr: parsing %s: %w", outPath, err)
	}

	return resolveFromTree(raw, wantIDs), nil
}

// dumpMprUnitTypes translates mx diff's type strings into the ones dump-mpr
// accepts, deduplicated, without mutating the caller's slice.
//
// ▶ Why: mx diff (like analyze-mpr) reports pages and their relatives under
// the STORAGE-level namespace — Forms$Page, Forms$Snippet, Forms$Layout,
// Forms$BuildingBlock, Forms$PageTemplate — while dump-mpr only accepts the
// public metamodel name Pages$X and rejects the whole call (exit 2,
// "Invalid unit type(s) 'Forms$Page'") on a single unknown one. All five
// confirmed against mx 11.13.0 on 2026-10-22; Pages$X is accepted for each.
// Only the filter is translated: the units come back under their dump-mpr
// type, but resolution matches on $ID, so the caller never notices.
func dumpMprUnitTypes(unitTypes []string) []string {
	seen := make(map[string]bool, len(unitTypes))
	out := make([]string, 0, len(unitTypes))
	for _, t := range unitTypes {
		if rest, ok := strings.CutPrefix(t, "Forms$"); ok {
			t = "Pages$" + rest
		}
		if !seen[t] {
			seen[t] = true
			out = append(out, t)
		}
	}
	return out
}

// parseInvalidUnitTypes reads the rejected types out of dump-mpr's exit-2
// message. Real captured output (mx 11.13.0, on stderr, one line):
//
//	Invalid unit type(s) 'Forms$Page, Nonsense$X'
//
// Returns nil when the message does not have that shape.
func parseInvalidUnitTypes(stderr string) []string {
	_, rest, ok := strings.Cut(stderr, "Invalid unit type(s) '")
	if !ok {
		return nil
	}
	list, _, ok := strings.Cut(rest, "'")
	if !ok {
		return nil
	}
	var out []string
	for _, t := range strings.Split(list, ",") {
		if t = strings.TrimSpace(t); t != "" {
			out = append(out, t)
		}
	}
	return out
}

func withoutTypes(types, drop []string) []string {
	out := make([]string, 0, len(types))
	for _, t := range types {
		if !slices.Contains(drop, t) {
			out = append(out, t)
		}
	}
	return out
}

// ensureModuleType returns unitTypes with "Projects$Module" appended if not
// already present, without mutating the caller's slice. The module unit is
// always the base case for synthesizeQualifiedName's container-chain walk —
// every document's containment chain terminates at its module — so without
// it, the chain-walk fallback for module-level singletons (DomainModel,
// ModuleSettings, ModuleSecurity, Folder — none of which dump-mpr gives a
// $QualifiedName) can never resolve, no matter what the caller's own
// unit-type filter contains.
func ensureModuleType(unitTypes []string) []string {
	for _, t := range unitTypes {
		if t == "Projects$Module" {
			return unitTypes
		}
	}
	out := make([]string, len(unitTypes), len(unitTypes)+1)
	copy(out, unitTypes)
	return append(out, "Projects$Module")
}

// resolveFromTree indexes every $ID-bearing node in a generically-decoded
// dump-mpr JSON tree, then resolves each id in wantIDs that was actually
// found — directly from its own $QualifiedName when it has one, otherwise
// via synthesizeQualifiedName. An id in wantIDs that dump-mpr didn't return
// at all (wrong unit type requested, or genuinely absent) is silently
// omitted from the result — degrade, don't fail; the caller decides how to
// present an unresolved id.
func resolveFromTree(raw any, wantIDs map[string]bool) map[string]ResolvedUnit {
	byID := make(map[string]map[string]any)
	indexByID(raw, byID)

	found := make(map[string]ResolvedUnit)
	memo := make(map[string]string)
	for id := range wantIDs {
		node, ok := byID[id]
		if !ok {
			continue
		}
		typ, _ := node["$Type"].(string)

		if qname, ok := node["$QualifiedName"].(string); ok && qname != "" {
			found[id] = ResolvedUnit{ID: id, Type: typ, QualifiedName: qname, Module: moduleOf(qname)}
			continue
		}

		if qname, ok := synthesizeQualifiedName(id, byID, memo, 0); ok {
			found[id] = ResolvedUnit{
				ID:                       id,
				Type:                     typ,
				QualifiedName:            qname,
				Module:                   moduleOf(qname),
				QualifiedNameSynthesized: true,
			}
		}
		// else: containment chain didn't resolve (missing ancestor, cycle,
		// or too deep) — omit rather than guess.
	}
	return found
}

// indexByID recursively walks a generically-decoded dump-mpr JSON tree and
// records every object carrying $ID, keyed by that id — regardless of
// whether it also has $QualifiedName, and regardless of nesting depth or
// the tree's top-level shape (dump-mpr's real top level is
// {"units": [...]}, not a bare array or the document itself, but this
// walk doesn't care either way since it recurses into every map/list).
func indexByID(node any, byID map[string]map[string]any) {
	switch v := node.(type) {
	case map[string]any:
		if id, ok := v["$ID"].(string); ok {
			byID[id] = v
		}
		for _, child := range v {
			indexByID(child, byID)
		}
	case []any:
		for _, child := range v {
			indexByID(child, byID)
		}
	}
}

// synthesizeQualifiedName builds a best-effort qualified name for a node
// dump-mpr did not give one to directly, by walking $ContainerID up the
// tree via byID until it reaches an ancestor that does have a real
// $QualifiedName (normally the module itself — see ensureModuleType).
// Each hop contributes one path segment: the node's own "name" field when
// present (e.g. a Projects$Folder named "Resources"), else its
// $ContainerProperty — the property slot it occupies on its parent, which
// dump-mpr sets on essentially every node (e.g. "domainModel",
// "moduleSettings", "moduleSecurity" for the three nameless per-module
// singletons confirmed in real output), else its own $Type as a last
// resort.
//
// memo caches resolved ids across calls (many siblings, e.g. folders,
// share the same ancestor chain) and, together with the depth counter,
// guards against a cyclic or malformed $ContainerID chain.
func synthesizeQualifiedName(id string, byID map[string]map[string]any, memo map[string]string, depth int) (string, bool) {
	if depth > maxContainerChainDepth {
		return "", false
	}
	if qname, ok := memo[id]; ok {
		return qname, true
	}

	node, ok := byID[id]
	if !ok {
		return "", false
	}

	if qname, ok := node["$QualifiedName"].(string); ok && qname != "" {
		memo[id] = qname
		return qname, true
	}

	containerID, _ := node["$ContainerID"].(string)
	if containerID == "" {
		return "", false
	}
	parentQName, ok := synthesizeQualifiedName(containerID, byID, memo, depth+1)
	if !ok {
		return "", false
	}

	segment, _ := node["name"].(string)
	if segment == "" {
		segment, _ = node["$ContainerProperty"].(string)
	}
	if segment == "" {
		segment, _ = node["$Type"].(string)
	}
	if segment == "" {
		return "", false
	}

	qname := parentQName + "." + segment
	memo[id] = qname
	return qname, true
}

func moduleOf(qname string) string {
	if i := strings.Index(qname, "."); i >= 0 {
		return qname[:i]
	}
	return qname
}