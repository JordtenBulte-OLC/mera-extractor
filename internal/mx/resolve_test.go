// internal/mx/resolve_test.go
package mx

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// ---- synthetic tree tests: indexByID / resolveFromTree / synthesizeQualifiedName ----

func TestResolveFromTree_DirectQualifiedName(t *testing.T) {
	tree := map[string]any{
		"$ID":            "id-1",
		"$Type":          "Microflows$Microflow",
		"$QualifiedName": "MyModule.ACT_DoThing",
	}
	found := resolveFromTree(tree, map[string]bool{"id-1": true})

	got, ok := found["id-1"]
	if !ok {
		t.Fatal("id-1 not found")
	}
	want := ResolvedUnit{ID: "id-1", Type: "Microflows$Microflow", QualifiedName: "MyModule.ACT_DoThing", Module: "MyModule"}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveFromTree_SynthesizedFromName(t *testing.T) {
	// A folder (has "name", no $QualifiedName) contained directly in a
	// module (has $QualifiedName, no $ContainerID) — mirrors the real
	// Projects$Folder / Projects$Module shape confirmed in the fixture.
	tree := map[string]any{
		"module": map[string]any{
			"$ID":            "module-1",
			"$Type":          "Projects$Module",
			"$QualifiedName": "MyModule",
		},
		"folder": map[string]any{
			"$ID":                "folder-1",
			"$Type":              "Projects$Folder",
			"$ContainerID":       "module-1",
			"$ContainerProperty": "folders",
			"name":               "Resources",
		},
	}
	found := resolveFromTree(tree, map[string]bool{"folder-1": true})

	got, ok := found["folder-1"]
	if !ok {
		t.Fatal("folder-1 not found")
	}
	want := ResolvedUnit{
		ID: "folder-1", Type: "Projects$Folder",
		QualifiedName: "MyModule.Resources", Module: "MyModule",
		QualifiedNameSynthesized: true,
	}
	if got != want {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestResolveFromTree_SynthesizedFromContainerProperty(t *testing.T) {
	// A node with neither $QualifiedName nor "name" (e.g.
	// DomainModels$DomainModel, Projects$ModuleSettings,
	// Security$ModuleSecurity in real output) falls back to
	// $ContainerProperty as its path segment.
	tree := map[string]any{
		"module": map[string]any{
			"$ID":            "module-1",
			"$Type":          "Projects$Module",
			"$QualifiedName": "MyModule",
		},
		"domainModel": map[string]any{
			"$ID":                "dm-1",
			"$Type":              "DomainModels$DomainModel",
			"$ContainerID":       "module-1",
			"$ContainerProperty": "domainModel",
		},
	}
	found := resolveFromTree(tree, map[string]bool{"dm-1": true})

	got, ok := found["dm-1"]
	if !ok {
		t.Fatal("dm-1 not found")
	}
	if !got.QualifiedNameSynthesized {
		t.Error("QualifiedNameSynthesized = false, want true")
	}
	if got.QualifiedName != "MyModule.domainModel" {
		t.Errorf("QualifiedName = %q, want %q", got.QualifiedName, "MyModule.domainModel")
	}
}

func TestResolveFromTree_SynthesizedFromTypeAsLastResort(t *testing.T) {
	// Neither $QualifiedName, "name", nor $ContainerProperty present —
	// falls all the way back to $Type. Not observed in real dump-mpr
	// output (every node there carries $ContainerProperty), but the
	// fallback chain should still degrade gracefully rather than omit.
	tree := map[string]any{
		"module": map[string]any{
			"$ID":            "module-1",
			"$QualifiedName": "MyModule",
		},
		"weird": map[string]any{
			"$ID":          "weird-1",
			"$Type":        "Some$WeirdType",
			"$ContainerID": "module-1",
		},
	}
	found := resolveFromTree(tree, map[string]bool{"weird-1": true})

	got, ok := found["weird-1"]
	if !ok {
		t.Fatal("weird-1 not found")
	}
	if got.QualifiedName != "MyModule.Some$WeirdType" {
		t.Errorf("QualifiedName = %q, want %q", got.QualifiedName, "MyModule.Some$WeirdType")
	}
}

func TestResolveFromTree_MultiLevelNesting(t *testing.T) {
	// module (qname) <- folder A (name only) <- folder B (name only).
	// Composing two synthesized hops in sequence, not just one.
	tree := []any{
		map[string]any{"$ID": "module-1", "$QualifiedName": "MyModule"},
		map[string]any{"$ID": "folderA", "$ContainerID": "module-1", "name": "Outer"},
		map[string]any{"$ID": "folderB", "$ContainerID": "folderA", "name": "Inner"},
	}
	found := resolveFromTree(tree, map[string]bool{"folderB": true})

	got, ok := found["folderB"]
	if !ok {
		t.Fatal("folderB not found")
	}
	if got.QualifiedName != "MyModule.Outer.Inner" {
		t.Errorf("QualifiedName = %q, want %q", got.QualifiedName, "MyModule.Outer.Inner")
	}
	if got.Module != "MyModule" {
		t.Errorf("Module = %q, want %q", got.Module, "MyModule")
	}
}

func TestResolveFromTree_CyclicContainerChain(t *testing.T) {
	// A -> B -> A. Must not hang or panic, and must simply omit the id —
	// maxContainerChainDepth is the backstop for this.
	tree := []any{
		map[string]any{"$ID": "a", "$ContainerID": "b", "name": "A"},
		map[string]any{"$ID": "b", "$ContainerID": "a", "name": "B"},
	}

	done := make(chan map[string]ResolvedUnit, 1)
	go func() { done <- resolveFromTree(tree, map[string]bool{"a": true}) }()

	select {
	case found := <-done:
		if _, ok := found["a"]; ok {
			t.Error("cyclic chain resolved to a qualified name — should have been omitted")
		}
	case <-timeoutC(t):
		t.Fatal("resolveFromTree hung on a cyclic $ContainerID chain")
	}
}

func TestResolveFromTree_MissingAncestor(t *testing.T) {
	// $ContainerID points at an id that was never indexed at all.
	tree := map[string]any{
		"$ID":          "orphan",
		"$ContainerID": "does-not-exist",
		"name":         "Orphan",
	}
	found := resolveFromTree(tree, map[string]bool{"orphan": true})
	if _, ok := found["orphan"]; ok {
		t.Error("orphan with a missing ancestor resolved anyway — should have been omitted")
	}
}

func TestResolveFromTree_EmptyContainerID(t *testing.T) {
	// $ContainerID present as a key but empty (as opposed to absent or
	// pointing at a genuinely missing id) — the chain simply has nowhere
	// to go, so this must omit rather than panic.
	tree := map[string]any{
		"$ID":          "orphan",
		"$ContainerID": "",
		"name":         "Orphan",
	}
	found := resolveFromTree(tree, map[string]bool{"orphan": true})
	if _, ok := found["orphan"]; ok {
		t.Error("empty $ContainerID resolved anyway — should have been omitted")
	}
}

func TestResolveFromTree_NoUsableSegment(t *testing.T) {
	// Ancestor resolves fine, but the node itself has no "name", no
	// $ContainerProperty, and no $Type — every fallback in the segment
	// chain is exhausted, so this must omit rather than produce a
	// qualified name ending in a bare ".".
	tree := map[string]any{
		"module": map[string]any{"$ID": "module-1", "$QualifiedName": "MyModule"},
		"bare":   map[string]any{"$ID": "bare-1", "$ContainerID": "module-1"},
	}
	found := resolveFromTree(tree, map[string]bool{"bare-1": true})
	if _, ok := found["bare-1"]; ok {
		t.Error("a node with no name/$ContainerProperty/$Type resolved anyway — should have been omitted")
	}
}

func TestResolveFromTree_WantedIDNotPresent(t *testing.T) {
	tree := map[string]any{"$ID": "id-1", "$QualifiedName": "MyModule.Thing"}
	found := resolveFromTree(tree, map[string]bool{"id-1": true, "never-seen": true})

	if len(found) != 1 {
		t.Fatalf("len(found) = %d, want 1", len(found))
	}
	if _, ok := found["never-seen"]; ok {
		t.Error("an id absent from the tree should not appear in the result map")
	}
}

func TestResolveFromTree_NonStringIDsAndQualifiedNamesIgnored(t *testing.T) {
	// dump-mpr should never actually emit non-string $ID/$QualifiedName,
	// but a generic `any` decode makes no such guarantee for arbitrary
	// JSON — defends against a panic from a bad type assertion.
	tree := map[string]any{
		"$ID":            float64(12345),
		"$QualifiedName": true,
		"nested": map[string]any{
			"$ID":            "real-id",
			"$QualifiedName": "MyModule.Real",
		},
	}
	found := resolveFromTree(tree, map[string]bool{"real-id": true})
	if _, ok := found["real-id"]; !ok {
		t.Fatal("real-id not found despite malformed sibling data")
	}
}

func TestEnsureModuleType(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"appends when absent", []string{"Microflows$Microflow"}, []string{"Microflows$Microflow", "Projects$Module"}},
		{"no duplicate when already present", []string{"Projects$Module", "Pages$Page"}, []string{"Projects$Module", "Pages$Page"}},
		{"empty input", nil, []string{"Projects$Module"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ensureModuleType(tc.in)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Errorf("ensureModuleType(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func TestEnsureModuleType_DoesNotMutateCallerSlice(t *testing.T) {
	in := []string{"Microflows$Microflow"}
	_ = ensureModuleType(in)
	if len(in) != 1 || in[0] != "Microflows$Microflow" {
		t.Errorf("caller's slice was mutated: %v", in)
	}
}

// timeoutC returns a channel that fires after a short deadline, used only
// to bound TestResolveFromTree_CyclicContainerChain in case
// maxContainerChainDepth regresses to something unbounded.
func timeoutC(t *testing.T) <-chan struct{} {
	t.Helper()
	ch := make(chan struct{})
	go func() {
		// generous but finite — a correct implementation returns in
		// microseconds; this only guards against a true hang.
		<-time.After(5 * time.Second)
		close(ch)
	}()
	return ch
}

// ---- real captured fixture: testdata/dump-reviewmanagement-trim.json ----
//
// A trimmed `mx dump-mpr` export of the ReviewManagement module — one item
// of each document type present in that module (confirmed with Jord: NOT
// exhaustive of every Mendix unit type; e.g. no published REST service).
// Confirmed against this file: real top-level shape is
// {"units": [...]} (not a bare array), a UTF-8 BOM prefix is present, and
// exactly 5 of the top-level units carry $ID with no $QualifiedName at all
// (2x Projects$Folder, DomainModels$DomainModel, Projects$ModuleSettings,
// Security$ModuleSecurity) — the real-world case
// QualifiedNameSynthesized exists for.
func TestResolveFromTree_RealCapturedFixture(t *testing.T) {
	data, err := os.ReadFile("testdata/dump-reviewmanagement-trim.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	data = stripBOM(data)
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatalf("parsing fixture: %v", err)
	}

	const (
		moduleID         = "13faa821-eb9e-46e1-9fa3-7ae6093728f7"
		folderAdminID    = "54ebfb4f-77fd-45eb-951d-9a82f2d773ee"
		folderResourceID = "d8a6ff8d-a81a-43d8-856b-d79dd26df7c0"
		domainModelID    = "9075c28e-b0dc-408f-b8e5-1faf1854472e"
		moduleSettingsID = "fbb60824-eb06-45c6-b490-1fcd5a0fc180"
		moduleSecurityID = "0683b777-81f2-4bad-941f-fba508267eeb"
		pageID           = "9850c137-f7d5-47ac-a454-833fa18c0627"
		pageParamID      = "0d6beab8-c1d7-4d95-bfcf-05eb4dcbeb26"
		microflowID      = "85c03808-4478-4825-8e5d-56e782d54340"
		entityID         = "f3471e03-c919-45f1-9a60-d7f4c5415928"
		roleAdminID      = "feaa1096-d0b9-4ba7-a5e2-0a6714e4a76e"
		roleReviewerID   = "ab436db3-0427-4f67-a852-997a2ab843cf"
		enumValueID      = "b6a15425-384f-4709-ac2a-2feb453e1168"
		neverPresentID   = "00000000-0000-0000-0000-000000000000"
	)

	wantIDs := map[string]bool{
		moduleID: true, folderAdminID: true, folderResourceID: true,
		domainModelID: true, moduleSettingsID: true, moduleSecurityID: true,
		pageID: true, pageParamID: true, microflowID: true, entityID: true,
		roleAdminID: true, roleReviewerID: true, enumValueID: true,
		neverPresentID: true,
	}
	found := resolveFromTree(raw, wantIDs)

	cases := []struct {
		id         string
		wantType   string
		wantQName  string
		wantModule string
		wantSynth  bool
	}{
		{moduleID, "Projects$Module", "ReviewManagement", "ReviewManagement", false},
		{folderAdminID, "Projects$Folder", "ReviewManagement.AdminPages", "ReviewManagement", true},
		{folderResourceID, "Projects$Folder", "ReviewManagement.Resources", "ReviewManagement", true},
		{domainModelID, "DomainModels$DomainModel", "ReviewManagement.domainModel", "ReviewManagement", true},
		{moduleSettingsID, "Projects$ModuleSettings", "ReviewManagement.moduleSettings", "ReviewManagement", true},
		{moduleSecurityID, "Security$ModuleSecurity", "ReviewManagement.moduleSecurity", "ReviewManagement", true},
		{pageID, "Pages$Page", "ReviewManagement.MendixApplication_NewEdit", "ReviewManagement", false},
		{pageParamID, "Pages$PageParameter", "ReviewManagement.MendixApplication_NewEdit.App", "ReviewManagement", false},
		{microflowID, "Microflows$Microflow", "ReviewManagement.ACT_Review_CallClaude", "ReviewManagement", false},
		{entityID, "DomainModels$Entity", "ReviewManagement.MendixApplication", "ReviewManagement", false},
		{roleAdminID, "Security$ModuleRole", "ReviewManagement.Administrator", "ReviewManagement", false},
		{roleReviewerID, "Security$ModuleRole", "ReviewManagement.Reviewer", "ReviewManagement", false},
		{enumValueID, "Enumerations$EnumerationValue", "ReviewManagement.ENUM_MappingSource.AutoHigh", "ReviewManagement", false},
	}
	for _, tc := range cases {
		got, ok := found[tc.id]
		if !ok {
			t.Errorf("id %s (%s): not found in result", tc.id, tc.wantType)
			continue
		}
		if got.Type != tc.wantType || got.QualifiedName != tc.wantQName || got.Module != tc.wantModule || got.QualifiedNameSynthesized != tc.wantSynth {
			t.Errorf("id %s: got %+v, want Type=%s QualifiedName=%s Module=%s Synthesized=%v",
				tc.id, got, tc.wantType, tc.wantQName, tc.wantModule, tc.wantSynth)
		}
	}

	if _, ok := found[neverPresentID]; ok {
		t.Error("an id never present in the fixture resolved anyway")
	}
	if len(found) != len(cases) {
		t.Errorf("len(found) = %d, want %d (unexpected extra entries: check for id collisions)", len(found), len(cases))
	}
}

// ---- ResolveQualifiedNames: exit-code handling and end-to-end stub tests ----

// writeDumpMprStubMx creates a stub mx whose "dump-mpr" subcommand writes
// dumpJSON to the --output-file argument (the 6th positional arg for the
// "dump-mpr MPRPATH --unit-type TYPES --output-file OUTPATH" invocation
// shape ResolveQualifiedNames uses), and separately records the --unit-type
// value it was actually called with so tests can confirm
// ensureModuleType's injection reached the real invocation, not just the
// unit-tested helper in isolation. Mirrors writeDiffStubMx's structure.
func writeDumpMprStubMx(t *testing.T, dumpJSON, stderr string, exitCode int) (bin Binary, seenUnitTypesPath string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "mx")
	seenUnitTypesPath = filepath.Join(dir, "seen-unit-types.txt")

	script := "#!/bin/sh\n"
	script += `outPath="$6"` + "\n"
	script += fmt.Sprintf(`echo -n "$4" > %q`+"\n", seenUnitTypesPath)
	if dumpJSON != "" {
		script += "cat > \"$outPath\" <<'MERA_DUMP_EOF'\n" + dumpJSON + "\nMERA_DUMP_EOF\n"
	}
	if stderr != "" {
		script += "cat <<'MERA_STDERR_EOF' >&2\n" + stderr + "\nMERA_STDERR_EOF\n"
	}
	script += fmt.Sprintf("exit %d\n", exitCode)

	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatalf("writeDumpMprStubMx: %v", err)
	}
	return Binary{Version: "stub", Path: path}, seenUnitTypesPath
}

const sampleDumpJSON = `{"units":[{"$ID":"id-1","$Type":"Microflows$Microflow","$QualifiedName":"MyModule.ACT_Thing"}]}`

func TestResolveQualifiedNames_Success(t *testing.T) {
	bin, seenTypesPath := writeDumpMprStubMx(t, sampleDumpJSON, "", 0)

	found, err := ResolveQualifiedNames(context.Background(), bin, "app.mpr", []string{"Microflows$Microflow"}, map[string]bool{"id-1": true})
	if err != nil {
		t.Fatalf("ResolveQualifiedNames: unexpected error: %v", err)
	}
	got, ok := found["id-1"]
	if !ok {
		t.Fatal("id-1 not found")
	}
	if got.QualifiedName != "MyModule.ACT_Thing" {
		t.Errorf("QualifiedName = %q, want %q", got.QualifiedName, "MyModule.ACT_Thing")
	}

	seen, err := os.ReadFile(seenTypesPath)
	if err != nil {
		t.Fatalf("reading seen unit types: %v", err)
	}
	if !strings.Contains(string(seen), "Projects$Module") {
		t.Errorf("--unit-type value %q did not include the auto-injected Projects$Module", seen)
	}
	if !strings.Contains(string(seen), "Microflows$Microflow") {
		t.Errorf("--unit-type value %q lost the caller's requested type", seen)
	}
}

func TestResolveQualifiedNames_ExitCode1(t *testing.T) {
	bin, _ := writeDumpMprStubMx(t, "", "wrong project file", 1)
	_, err := ResolveQualifiedNames(context.Background(), bin, "app.mpr", nil, nil)
	assertDumpMprExitError(t, err, 1, "wrong project file provided")
}

func TestResolveQualifiedNames_ExitCode2(t *testing.T) {
	bin, _ := writeDumpMprStubMx(t, "", "bad unit type", 2)
	_, err := ResolveQualifiedNames(context.Background(), bin, "app.mpr", nil, nil)
	assertDumpMprExitError(t, err, 2, "invalid unit type(s)")
}

func TestResolveQualifiedNames_ExitCode3(t *testing.T) {
	bin, _ := writeDumpMprStubMx(t, "", "export blew up", 3)
	_, err := ResolveQualifiedNames(context.Background(), bin, "app.mpr", nil, nil)
	assertDumpMprExitError(t, err, 3, "unknown JSON export error")
}

func TestResolveQualifiedNames_ExitCode4(t *testing.T) {
	bin, _ := writeDumpMprStubMx(t, "", "different mendix version", 4)
	_, err := ResolveQualifiedNames(context.Background(), bin, "app.mpr", nil, nil)
	assertDumpMprExitError(t, err, 4, "project is in a different Mendix version")
}

func TestResolveQualifiedNames_UnexpectedExitCode(t *testing.T) {
	bin, _ := writeDumpMprStubMx(t, "", "??", 99)
	_, err := ResolveQualifiedNames(context.Background(), bin, "app.mpr", nil, nil)
	assertDumpMprExitError(t, err, 99, "unexpected exit code")
}

func assertDumpMprExitError(t *testing.T, err error, wantCode int, wantMeaning string) {
	t.Helper()
	if err == nil {
		t.Fatal("ResolveQualifiedNames returned no error")
	}
	var target *DumpMprExitError
	if !errors.As(err, &target) {
		t.Fatalf("error type = %T, want *DumpMprExitError", err)
	}
	if target.ExitCode != wantCode {
		t.Errorf("ExitCode = %d, want %d", target.ExitCode, wantCode)
	}
	if !strings.Contains(err.Error(), wantMeaning) {
		t.Errorf("Error() = %q, missing meaning %q", err.Error(), wantMeaning)
	}
}

func TestResolveQualifiedNames_RealExecutionFailure(t *testing.T) {
	bin := Binary{Version: "missing", Path: "/definitely/does/not/exist/mx"}
	_, err := ResolveQualifiedNames(context.Background(), bin, "app.mpr", nil, nil)
	if err == nil {
		t.Fatal("ResolveQualifiedNames returned no error for a nonexistent binary")
	}
	var target *DumpMprExitError
	if errors.As(err, &target) {
		t.Error("a real execution failure should not be typed as *DumpMprExitError")
	}
}

func TestResolveQualifiedNames_MissingOutputFile(t *testing.T) {
	bin, _ := writeDumpMprStubMx(t, "", "", 0) // exit 0 but never writes outPath
	_, err := ResolveQualifiedNames(context.Background(), bin, "app.mpr", nil, map[string]bool{"id-1": true})
	if err == nil {
		t.Fatal("ResolveQualifiedNames returned no error when outPath was never written")
	}
}

func TestResolveQualifiedNames_MalformedJSON(t *testing.T) {
	bin, _ := writeDumpMprStubMx(t, "{not valid json", "", 0)
	_, err := ResolveQualifiedNames(context.Background(), bin, "app.mpr", nil, map[string]bool{"id-1": true})
	if err == nil {
		t.Fatal("ResolveQualifiedNames returned no error for malformed JSON")
	}
}

func TestResolveQualifiedNames_HandlesUTF8BOM(t *testing.T) {
	bomPrefixed := "\ufeff" + sampleDumpJSON
	bin, _ := writeDumpMprStubMx(t, bomPrefixed, "", 0)

	found, err := ResolveQualifiedNames(context.Background(), bin, "app.mpr", []string{"Microflows$Microflow"}, map[string]bool{"id-1": true})
	if err != nil {
		t.Fatalf("ResolveQualifiedNames: unexpected error with BOM-prefixed output: %v", err)
	}
	if _, ok := found["id-1"]; !ok {
		t.Fatal("id-1 not found in BOM-prefixed output")
	}
}

// TestResolveQualifiedNames_EndToEndWithRealFixture drives the full
// function (not just resolveFromTree) through the stub, using the real
// fixture as the dump-mpr output — confirming the BOM strip + JSON parse +
// index + resolve pipeline works together end to end, not just each piece
// in isolation.
func TestResolveQualifiedNames_EndToEndWithRealFixture(t *testing.T) {
	fixture, err := os.ReadFile("testdata/dump-reviewmanagement-trim.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	bin, _ := writeDumpMprStubMx(t, string(fixture), "", 0)

	const domainModelID = "9075c28e-b0dc-408f-b8e5-1faf1854472e"
	found, err := ResolveQualifiedNames(context.Background(), bin, "app.mpr",
		[]string{"DomainModels$DomainModel"}, map[string]bool{domainModelID: true})
	if err != nil {
		t.Fatalf("ResolveQualifiedNames: unexpected error: %v", err)
	}
	got, ok := found[domainModelID]
	if !ok {
		t.Fatal("domain model id not found")
	}
	if got.QualifiedName != "ReviewManagement.domainModel" || !got.QualifiedNameSynthesized {
		t.Errorf("got %+v, want synthesized QualifiedName ReviewManagement.domainModel", got)
	}
}