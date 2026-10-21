// internal/api/extract_integration_test.go
package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"mera-extractor/internal/gitops"
	"mera-extractor/internal/mx"
)

// Guarded end-to-end test — real Team Server, real mx binaries, real mxcli.
// Nothing is faked: the Server is built by NewServer, so every Deps field
// falls back to the real implementation.
//
//	set -a; source .env; set +a          # KEY=value, no spaces around the =
//	export MERA_PAT="..."                # or: source ~/.mera-secrets.sh
//	export MERA_MX_ROOT="$HOME/mera/extractor/.mx-binaries"
//	export MERA_IT_REPO="https://git.api.mendix.com/b12ab91d-....git"
//	export MERA_IT_BASE_SHA="..."
//	export MERA_IT_HEAD_SHA="..."
//	go test ./internal/api/ -run Integration -v
//
// mxcli must also be on PATH. Absent any of the variables it skips, so
// `go test ./...` stays hermetic and offline.
//
// Read the logged output, not just the pass/fail: the "diff types seen" table
// is how the UNVERIFIED entries in diffTypeToMxcli get confirmed or corrected.
func TestExtract_Integration(t *testing.T) {
	repoURL := os.Getenv("MERA_IT_REPO")
	pat := os.Getenv("MERA_PAT")
	baseSha := os.Getenv("MERA_IT_BASE_SHA")
	headSha := os.Getenv("MERA_IT_HEAD_SHA")
	if repoURL == "" || pat == "" || baseSha == "" || headSha == "" || os.Getenv("MERA_MX_ROOT") == "" {
		t.Skip("set MERA_IT_REPO, MERA_PAT, MERA_IT_BASE_SHA, MERA_IT_HEAD_SHA and MERA_MX_ROOT to run")
	}

	// Validated and made absolute BEFORE the 15-second clone, so a bad root
	// fails in milliseconds with a message that says what is wrong.
	mxRoot := resolveMxRoot(t)
	srv := NewServer(t.TempDir(), mxRoot)

	body, err := json.Marshal(extractRequest{
		RequestID: "integration-1",
		RepoURL:   repoURL,
		Username:  "pat",
		Pat:       pat,
		BaseSha:   baseSha,
		HeadSha:   headSha,
	})
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	srv.handleExtract(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", w.Code, w.Body.String())
	}

	// Keep the full payload on disk — it is the only convenient way to eyeball
	// real MDL, and it is far too big to read from test output.
	dump := filepath.Join(os.TempDir(), "mera-extract-integration.json")
	if err := os.WriteFile(dump, w.Body.Bytes(), 0o600); err == nil {
		t.Logf("full response written to %s (%d bytes)", dump, w.Body.Len())
	}

	var resp extractResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}

	// --- provenance -------------------------------------------------------
	t.Logf("mendixVersion=%q mxToolsetVersion=%q mxcliVersion=%q",
		resp.MendixVersion, resp.MxToolsetVersion, resp.MxcliVersion)
	if resp.MendixVersion == "" {
		t.Error("mendixVersion is empty — mx show-version did not report a version")
	}
	// A 200 already proves a binary was selected (a resolve failure is the 422
	// caught above). mxToolsetVersion is omitted in the normal exact-match
	// case; if it IS present it must genuinely differ from the app version.
	if resp.MxToolsetVersion != "" && resp.MxToolsetVersion == resp.MendixVersion {
		t.Errorf("mxToolsetVersion is set but equals mendixVersion — it should have been omitted")
	}
	if resp.RequestID != "integration-1" {
		t.Errorf("requestId = %q, not echoed back", resp.RequestID)
	}

	// --- the actual point of Stage 8 --------------------------------------
	if len(resp.ChangeUnits) == 0 {
		t.Fatal("no change units — either the SHAs are identical or mx diff found nothing")
	}
	// The test app has 608 units. A targeted extraction of a two-commit diff
	// must be nowhere near that; if it is, the naive path ran by mistake.
	if len(resp.ChangeUnits) > 100 {
		t.Errorf("%d change units looks like full enumeration, not a targeted diff", len(resp.ChangeUnits))
	}
	t.Logf("%d change units", len(resp.ChangeUnits))

	// --- the table worth reading ------------------------------------------
	// One row per distinct mx diff type, with how many resolved a name and how
	// many rendered MDL. A type showing units>0 but mdl=0 is either a genuine
	// knownNotDescribable or a missing diffTypeToMxcli entry — the warnings
	// below say which.
	type stat struct{ units, named, withMdl, synthesized int }
	byType := map[string]*stat{}
	for _, u := range resp.ChangeUnits {
		s := byType[u.UnitType]
		if s == nil {
			s = &stat{}
			byType[u.UnitType] = s
		}
		s.units++
		if u.QualifiedName != "" {
			s.named++
		}
		if u.BeforeMdl != "" || u.AfterMdl != "" {
			s.withMdl++
		}
		if u.QualifiedNameSynthesized {
			s.synthesized++
		}
	}
	types := make([]string, 0, len(byType))
	for k := range byType {
		types = append(types, k)
	}
	sort.Strings(types)
	t.Log("diff types seen (units / named / with MDL / synthesized name):")
	for _, k := range types {
		s := byType[k]
		t.Logf("    %-40s %3d / %3d / %3d / %3d", k, s.units, s.named, s.withMdl, s.synthesized)
	}

	// --- per-unit assertions ----------------------------------------------
	var named, withMdl int
	for _, u := range resp.ChangeUnits {
		if u.ChangeKind != "Added" && u.ChangeKind != "Modified" && u.ChangeKind != "Deleted" {
			t.Errorf("unexpected changeKind %q on %s", u.ChangeKind, u.QualifiedName)
		}
		if u.UnitType == "" {
			t.Error("a change unit came back with no unitType")
		}
		if u.QualifiedName != "" {
			named++
			if u.Module == "" {
				t.Errorf("%s resolved a name but no module", u.QualifiedName)
			}
		}
		if u.BeforeMdl != "" || u.AfterMdl != "" {
			withMdl++
			if u.TokenEstimate <= 0 {
				t.Errorf("%s has MDL but a zero tokenEstimate", u.QualifiedName)
			}
		}
		// The Step 8 contract, restated as an assertion.
		if u.ChangeKind == "Added" && u.BeforeMdl != "" {
			t.Errorf("%s is Added but carries beforeMdl", u.QualifiedName)
		}
		if u.ChangeKind == "Deleted" && u.AfterMdl != "" {
			t.Errorf("%s is Deleted but carries afterMdl", u.QualifiedName)
		}
		if u.PreviousQualifiedName != "" {
			t.Logf("rename detected: %s → %s", u.PreviousQualifiedName, u.QualifiedName)
		}
	}
	if named == 0 {
		t.Error("not one unit resolved a qualified name — dump-mpr resolution is broken")
	}
	if withMdl == 0 {
		t.Error("not one unit rendered MDL — the describe stage never fired")
	}
	t.Logf("%d/%d units named, %d rendered MDL", named, len(resp.ChangeUnits), withMdl)

	// MERA_IT_EXPECT_UNIT pins a known qualified name for a known commit pair —
	// e.g. MxCliExtractor.ACT_Test_newMicroflow for the microflow-add pair.
	if want := os.Getenv("MERA_IT_EXPECT_UNIT"); want != "" {
		var found bool
		for _, u := range resp.ChangeUnits {
			if u.QualifiedName == want {
				found = true
				if u.AfterMdl == "" && u.ChangeKind != "Deleted" {
					t.Errorf("%s was found but rendered no MDL", want)
				}
				t.Logf("expected unit %s: changeKind=%s mdl=%d bytes", want, u.ChangeKind, len(u.AfterMdl))
			}
		}
		if !found {
			t.Errorf("expected unit %q not among the change units", want)
		}
	}

	// --- textDiffs (Step 10) ----------------------------------------------
	t.Logf("%d text diffs", len(resp.TextDiffs))
	for _, td := range resp.TextDiffs {
		if td.Path == "" || td.ChangeKind == "" {
			t.Errorf("malformed text diff: %+v", td)
		}
		t.Logf("    %-8s %s (%d bytes)", td.ChangeKind, td.Path, len(td.UnifiedDiff))
	}

	// --- warnings: the actionable output ----------------------------------
	for _, warn := range resp.Warnings {
		t.Logf("WARNING: %s", warn)
	}
	// Unrecognised types are the one warning class that means "go edit the
	// map". Surfaced as a failure so it can't be scrolled past — flip to
	// t.Logf if you would rather collect them across several runs first.
	for _, warn := range resp.Warnings {
		if strings.Contains(warn, "unrecognised change unit type") {
			t.Errorf("ACTION: %s", warn)
		}
	}
}

// ---------------------------------------------------------------------------
// MERA_MX_ROOT resolution
// ---------------------------------------------------------------------------

// resolveMxRoot turns MERA_MX_ROOT into a validated absolute directory, and
// fails immediately with a diagnosis if it isn't one.
//
// A RELATIVE value is the trap this exists for. `go test` runs each package's
// test binary with THAT PACKAGE'S SOURCE DIRECTORY as the working directory —
// not the module root, and not wherever you typed `go test ./...`. So
// `MERA_MX_ROOT=./.mx-binaries` resolves to internal/api/.mx-binaries here,
// internal/mx/.mx-binaries over there, and the repo root only if you happen to
// run the root package. It fails differently depending on which package you
// run, which is about the least helpful failure mode available.
//
// Rather than banning relative paths, resolve them against the MODULE ROOT
// (the nearest ancestor with a go.mod), which is what anyone typing
// "./.mx-binaries" actually means. Absolute paths are used as given.
func resolveMxRoot(t *testing.T) string {
	t.Helper()
	raw := os.Getenv("MERA_MX_ROOT")

	// "~" is expanded by the shell, never by Go — a quoted '~/...' in a .env
	// arrives here literally and would otherwise create a directory named "~".
	if raw == "~" || strings.HasPrefix(raw, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			t.Fatalf("MERA_MX_ROOT starts with ~ but the home directory is unknown: %v", err)
		}
		raw = filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(raw, "~"), "/"))
	}

	abs := raw
	if !filepath.IsAbs(abs) {
		root, err := moduleRoot()
		if err != nil {
			t.Fatalf("MERA_MX_ROOT %q is relative and the module root could not be found: %v", raw, err)
		}
		abs = filepath.Join(root, raw)
		t.Logf("MERA_MX_ROOT %q is relative; resolved against the module root to %s", raw, abs)
	}
	abs = filepath.Clean(abs)

	cwd, _ := os.Getwd()
	fi, err := os.Stat(abs)
	if err != nil || !fi.IsDir() {
		t.Fatalf("MERA_MX_ROOT %q → %s is not a directory (test cwd is %s).\n"+
			"Prefer an absolute path in .env, e.g. MERA_MX_ROOT=$HOME/mera/extractor/.mx-binaries",
			raw, abs, cwd)
	}

	// mx.Resolve expects <root>/<version>/modeler/mx — one level deeper than
	// the manual's diagram. Catching that here beats a 500 fifteen seconds in.
	matches, _ := filepath.Glob(filepath.Join(abs, "*", "modeler", "mx"))
	if len(matches) == 0 {
		entries, _ := os.ReadDir(abs)
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Fatalf("no <version>/modeler/mx under %s; it contains %v.\n"+
			"Check with: ls -d %q/*/modeler/mx", abs, names, abs)
	}
	t.Logf("mx binaries: %v", matches)
	return abs
}

// moduleRoot walks up from the working directory to the nearest go.mod.
func moduleRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no go.mod in any parent directory")
		}
		dir = parent
	}
}

// Confirms the escape hatch still behaves against the real repo — it is the
// fallback if the diff path ever misbehaves in production, so it should not be
// allowed to rot.
func TestExtract_IntegrationEscapeHatch(t *testing.T) {
	repoURL := os.Getenv("MERA_IT_REPO")
	pat := os.Getenv("MERA_PAT")
	baseSha := os.Getenv("MERA_IT_BASE_SHA")
	headSha := os.Getenv("MERA_IT_HEAD_SHA")
	if repoURL == "" || pat == "" || baseSha == "" || headSha == "" {
		t.Skip("set MERA_IT_REPO, MERA_PAT, MERA_IT_BASE_SHA and MERA_IT_HEAD_SHA to run")
	}

	// The naive path never touches an mx binary, so an unset MERA_MX_ROOT is
	// fine here — but validate it when it IS set, rather than passing a broken
	// root through and hoping nothing reaches for it.
	var mxRoot string
	if os.Getenv("MERA_MX_ROOT") != "" {
		mxRoot = resolveMxRoot(t)
	}
	srv := NewServer(t.TempDir(), mxRoot)

	body, _ := json.Marshal(extractRequest{
		RepoURL: repoURL, Username: "pat", Pat: pat,
		BaseSha: baseSha, HeadSha: headSha,
		Modules: []string{"Administration"}, // ~19 units, small enough to be quick
	})
	r := httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	srv.handleExtract(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", w.Code, w.Body.String())
	}
	var legacy legacyExtractResponse
	if err := json.Unmarshal(w.Body.Bytes(), &legacy); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(legacy.Units) == 0 {
		t.Error("naive enumeration of Administration returned nothing")
	}
	if !strings.HasSuffix(legacy.MprPath, ".mpr") {
		t.Errorf("mprPath = %q", legacy.MprPath)
	}
	// It must be the HEAD worktree's .mpr.
	if !strings.Contains(legacy.MprPath, string(filepath.Separator)+"head"+string(filepath.Separator)) {
		t.Errorf("mprPath %q is not in the head worktree", legacy.MprPath)
	}
	t.Logf("%d units enumerated from %s", len(legacy.Units), legacy.MprPath)
}

// Guarded like TestExtract_Integration, plus a commit pair that changes at
// least one microflow AND one page:
//
//	export MERA_IT_NAMES_BASE_SHA=13677c2...   # on the b12ab91d test repo:
//	export MERA_IT_NAMES_HEAD_SHA=c340a97...   # 1 page, 2 microflows, 1 image
//
// The regression it pins (2026-10-22): mx diff reports a page as Forms$Page,
// dump-mpr only accepts Pages$Page and rejected the whole call, so every unit
// of every type came back nameless and MDL-less. It runs on the real sparse,
// model-only worktrees, so it would also catch mx or mxcli needing a file the
// sparse set leaves out — the first suspect for that symptom.
func TestExtract_SparseCheckoutNames_Integration(t *testing.T) {
	repoURL := os.Getenv("MERA_IT_REPO")
	pat := os.Getenv("MERA_PAT")
	baseSha := os.Getenv("MERA_IT_NAMES_BASE_SHA")
	headSha := os.Getenv("MERA_IT_NAMES_HEAD_SHA")
	if repoURL == "" || pat == "" || baseSha == "" || headSha == "" || os.Getenv("MERA_MX_ROOT") == "" {
		t.Skip("set MERA_IT_REPO, MERA_PAT, MERA_IT_NAMES_BASE_SHA, MERA_IT_NAMES_HEAD_SHA and MERA_MX_ROOT to run")
	}
	mxRoot := resolveMxRoot(t)
	srv := NewServer(t.TempDir(), mxRoot)

	// Real implementations throughout; the wrappers only record what the
	// handler used and keep the worktrees alive past the response, so the
	// DomainModel check below reads the very same sparse checkout.
	real := Deps{}.withDefaults()
	var clone gitops.CloneBothResult
	var headMpr string
	srv.Deps.CloneBoth = func(ctx context.Context, workRoot string, req gitops.CloneBothRequest) (gitops.CloneBothResult, error) {
		res, err := real.CloneBoth(ctx, workRoot, req)
		clone = res
		return res, err
	}
	srv.Deps.PrepareMpr = func(ctx context.Context, bin mx.Binary, dir string) (string, mx.AnalyzeResult, error) {
		p, info, err := real.PrepareMpr(ctx, bin, dir)
		if dir == clone.HeadDir {
			headMpr = p
		}
		return p, info, err
	}
	srv.Deps.Cleanup = func(string) error { return nil }
	t.Cleanup(func() { real.Cleanup(clone.WorkDir) })

	body, _ := json.Marshal(extractRequest{
		RequestID: "integration-names", RepoURL: repoURL, Username: "pat", Pat: pat,
		BaseSha: baseSha, HeadSha: headSha,
	})
	w := httptest.NewRecorder()
	srv.handleExtract(w, httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(string(body))))
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d\n%s", w.Code, w.Body.String())
	}
	var resp extractResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, warn := range resp.Warnings {
		t.Logf("WARNING: %s", warn)
		if strings.Contains(warn, "could not resolve") {
			t.Errorf("name resolution failed: %s", warn)
		}
	}

	// --- the worktree really is sparse -------------------------------------
	entries, err := os.ReadDir(clone.HeadDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if n := e.Name(); n != ".git" && n != "mprcontents" && !strings.HasSuffix(n, ".mpr") {
			t.Errorf("head worktree contains %q — not the model-only sparse set this test is meant to cover", n)
		}
	}

	// --- Microflow and Page: name, module and MDL per side ------------------
	seen := map[string]int{}
	for _, u := range resp.ChangeUnits {
		kind := map[string]string{
			"Microflows$Microflow": "microflow", "Forms$Page": "page", "Pages$Page": "page",
		}[u.UnitType]
		if kind == "" {
			continue
		}
		seen[kind]++
		t.Logf("%-9s %-8s %s before=%dB after=%dB", kind, u.ChangeKind, u.QualifiedName, len(u.BeforeMdl), len(u.AfterMdl))
		if u.QualifiedName == "" || u.Module == "" {
			t.Errorf("%s %s unit has no name/module: %+v", u.ChangeKind, u.UnitType, u)
		}
		if u.ChangeKind != "Added" && u.BeforeMdl == "" {
			t.Errorf("%s %s has no beforeMdl", u.ChangeKind, u.QualifiedName)
		}
		if u.ChangeKind != "Deleted" && u.AfterMdl == "" {
			t.Errorf("%s %s has no afterMdl", u.ChangeKind, u.QualifiedName)
		}
	}
	if seen["microflow"] == 0 || seen["page"] == 0 {
		t.Fatalf("the commit pair must change a microflow and a page to cover the regression; saw %v", seen)
	}

	// --- DomainModel: name, on the same sparse head --------------------------
	// None changes in this pair, so the ids come straight from dump-mpr. The
	// type list leads with Forms$Page on purpose: before the fix, that alone
	// failed the call. MDL is deliberately not asserted — DomainModel is in
	// knownNotDescribable (mxcli's `domainmodel` is a diagram, not MDL).
	bin, err := mx.Resolve(mxRoot, resp.MendixVersion)
	if err != nil {
		t.Fatal(err)
	}
	dmIDs := dumpUnitIDs(t, bin, headMpr, "DomainModels$DomainModel")
	if len(dmIDs) == 0 {
		t.Fatal("dump-mpr found no domain models in the head worktree")
	}
	got, err := mx.ResolveQualifiedNames(context.Background(), bin, headMpr,
		[]string{"Forms$Page", "DomainModels$DomainModel"}, dmIDs)
	if err != nil {
		t.Fatalf("ResolveQualifiedNames: %v", err)
	}
	for id := range dmIDs {
		u, ok := got[id]
		if !ok || u.Module == "" || u.QualifiedName != u.Module+".domainModel" {
			t.Errorf("domain model %s resolved to %+v, want <Module>.domainModel", id, u)
		}
	}
	t.Logf("%d domain models named on the sparse checkout", len(got))
}

// dumpUnitIDs runs dump-mpr directly and returns the $ID of every top-level
// unit of unitType. Test-only discovery: production code never needs ids it
// was not handed by mx diff.
func dumpUnitIDs(t *testing.T, bin mx.Binary, mprPath, unitType string) map[string]bool {
	t.Helper()
	out := filepath.Join(t.TempDir(), "dump.json")
	if b, err := exec.Command(bin.Path, "dump-mpr", mprPath, "--unit-type", unitType, "--output-file", out).CombinedOutput(); err != nil {
		t.Fatalf("dump-mpr: %v\n%s", err, b)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	var dump struct {
		Units []struct {
			ID string `json:"$ID"`
		} `json:"units"`
	}
	if err := json.Unmarshal(bytes.TrimPrefix(data, []byte{0xEF, 0xBB, 0xBF}), &dump); err != nil {
		t.Fatalf("parse dump: %v", err)
	}
	ids := map[string]bool{}
	for _, u := range dump.Units {
		ids[u.ID] = true
	}
	return ids
}
