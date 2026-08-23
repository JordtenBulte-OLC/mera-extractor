// internal/api/extract_test.go
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"mera-extractor/internal/gitops"
	"mera-extractor/internal/mx"
	"mera-extractor/internal/mxcli"
)

// ---------------------------------------------------------------------------
// Harness
// ---------------------------------------------------------------------------

// describeCall records one mxcli.Describe invocation so a test can assert
// which .mpr and which qualified name it was pointed at — the whole point of
// the base/head split, and invisible from the response alone on a rename.
type describeCall struct {
	MprPath       string
	UnitType      string
	QualifiedName string
}

type harness struct {
	t       *testing.T
	srv     *Server
	workDir string
	baseMpr string
	headMpr string

	mu             sync.Mutex
	describeCalls  []describeCall
	resolveCalls   []string // mprPath per ResolveQualifiedNames call
	resolveTypes   [][]string
	cleanupCalls   []string
	listUnitsCalls int
	textDiffCalls  [][3]string // repoDir, baseSha, headSha
}

// newHarness wires a Server whose every external dependency is faked, and
// creates real directories for base/head so any code that touches the
// filesystem behaves normally.
func newHarness(t *testing.T) *harness {
	t.Helper()
	work := t.TempDir()
	baseDir := filepath.Join(work, "base")
	headDir := filepath.Join(work, "head")
	for _, d := range []string{baseDir, headDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "MERA.mpr"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	h := &harness{
		t:       t,
		workDir: work,
		baseMpr: filepath.Join(baseDir, "MERA.mpr"),
		headMpr: filepath.Join(headDir, "MERA.mpr"),
	}

	h.srv = &Server{
		WorkRoot: work,
		MxRoot:   "/opt/mx",
		Deps: Deps{
			CloneBoth: func(ctx context.Context, workRoot string, req gitops.CloneBothRequest) (gitops.CloneBothResult, error) {
				return gitops.CloneBothResult{
					WorkDir: work, RepoDir: filepath.Join(work, "repo.git"),
					BaseDir: baseDir, HeadDir: headDir,
				}, nil
			},
			TextDiffs: func(ctx context.Context, repoDir, baseSha, headSha string, pathspecs []string) ([]gitops.TextDiff, error) {
				h.mu.Lock()
				h.textDiffCalls = append(h.textDiffCalls, [3]string{repoDir, baseSha, headSha})
				h.mu.Unlock()
				return []gitops.TextDiff{{
					Path: "javasource/sales/Foo.java", ChangeKind: "Modified", UnifiedDiff: "@@ -1 +1 @@",
				}}, nil
			},
			Cleanup: func(workDir string) error {
				h.mu.Lock()
				defer h.mu.Unlock()
				h.cleanupCalls = append(h.cleanupCalls, workDir)
				return nil
			},
			FindMpr: func(dir string) (string, error) {
				return filepath.Join(dir, "MERA.mpr"), nil
			},
			MxHighest: func(mxRoot string) (mx.Binary, error) {
				return mx.Binary{Version: "11.13.0", Path: "/opt/mx/11.13.0/modeler/mx"}, nil
			},
			MxResolve: func(mxRoot, v string) (mx.Binary, error) {
				return mx.Binary{Version: v, Path: "/opt/mx/" + v + "/modeler/mx"}, nil
			},
			PrepareMpr: func(ctx context.Context, bin mx.Binary, dir string) (string, mx.AnalyzeResult, error) {
				return filepath.Join(dir, "MERA.mpr"), mx.AnalyzeResult{MendixVersion: "11.13.0"}, nil
			},
			Diff: func(ctx context.Context, bin mx.Binary, basePath, headPath, outPath string) (mx.DiffResult, error) {
				return mx.DiffResult{}, nil
			},
			ResolveQualifiedNames: func(ctx context.Context, bin mx.Binary, mprPath string, unitTypes []string, wantIDs map[string]bool) (map[string]mx.ResolvedUnit, error) {
				h.mu.Lock()
				h.resolveCalls = append(h.resolveCalls, mprPath)
				h.resolveTypes = append(h.resolveTypes, unitTypes)
				h.mu.Unlock()
				return map[string]mx.ResolvedUnit{}, nil
			},
			Describe: func(ctx context.Context, req mxcli.DescribeRequest) (string, error) {
				h.mu.Lock()
				h.describeCalls = append(h.describeCalls, describeCall{req.MprPath, req.UnitType, req.QualifiedName})
				h.mu.Unlock()
				return "MDL(" + req.QualifiedName + ")", nil
			},
			ListUnits: func(ctx context.Context, mprPath, unitType, module string) ([]mxcli.UnitSummary, error) {
				h.mu.Lock()
				h.listUnitsCalls++
				h.mu.Unlock()
				return []mxcli.UnitSummary{{Module: "Sales", Name: "X", QualifiedName: "Sales." + unitType}}, nil
			},
			MxcliVersion: func(ctx context.Context) (string, error) { return "v0.13.0", nil },
		},
	}
	return h
}

func (h *harness) post(body string) *httptest.ResponseRecorder {
	h.t.Helper()
	r := httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(body))
	w := httptest.NewRecorder()
	h.srv.handleExtract(w, r)
	return w
}

// postDefault issues a valid two-SHA request; every diff-path test uses it.
func (h *harness) postDefault() *httptest.ResponseRecorder {
	return h.post(`{"requestId":"req-1","repoUrl":"https://git.example/x.git","baseSha":"aaaaaaa","headSha":"bbbbbbb"}`)
}

func decodeExtract(t *testing.T, w *httptest.ResponseRecorder) extractResponse {
	t.Helper()
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body = %s", w.Code, w.Body.String())
	}
	var resp extractResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v\n%s", err, w.Body.String())
	}
	return resp
}

func (h *harness) setDiff(units ...mx.UnitDifference) {
	h.srv.Deps.Diff = func(ctx context.Context, bin mx.Binary, basePath, headPath, outPath string) (mx.DiffResult, error) {
		return mx.DiffResult{UnitDifferences: units}, nil
	}
}

// setNames makes ResolveQualifiedNames answer from two per-side tables keyed
// by id, so a rename is expressed simply by giving the two sides different
// names for the same id.
func (h *harness) setNames(base, head map[string]mx.ResolvedUnit) {
	h.srv.Deps.ResolveQualifiedNames = func(ctx context.Context, bin mx.Binary, mprPath string, unitTypes []string, wantIDs map[string]bool) (map[string]mx.ResolvedUnit, error) {
		h.mu.Lock()
		h.resolveCalls = append(h.resolveCalls, mprPath)
		h.resolveTypes = append(h.resolveTypes, unitTypes)
		h.mu.Unlock()

		src := head
		if mprPath == h.baseMpr {
			src = base
		}
		out := map[string]mx.ResolvedUnit{}
		for id := range wantIDs {
			if u, ok := src[id]; ok {
				out[id] = u
			}
		}
		return out, nil
	}
}

func mf(id, name string) mx.ResolvedUnit {
	return mx.ResolvedUnit{ID: id, Type: "Microflows$Microflow", QualifiedName: name, Module: strings.SplitN(name, ".", 2)[0]}
}

func findUnit(t *testing.T, resp extractResponse, qualifiedName string) changeUnit {
	t.Helper()
	for _, u := range resp.ChangeUnits {
		if u.QualifiedName == qualifiedName {
			return u
		}
	}
	t.Fatalf("no change unit named %q in %+v", qualifiedName, resp.ChangeUnits)
	return changeUnit{}
}

func hasWarning(resp extractResponse, substr string) bool {
	for _, w := range resp.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Request validation and plumbing
// ---------------------------------------------------------------------------

func TestExtract_RejectsMissingShas(t *testing.T) {
	for _, body := range []string{
		`{"repoUrl":"https://git.example/x.git","headSha":"bbbbbbb"}`,
		`{"repoUrl":"https://git.example/x.git","baseSha":"aaaaaaa"}`,
		`{"repoUrl":"https://git.example/x.git"}`,
	} {
		h := newHarness(t)
		w := h.post(body)
		if w.Code != http.StatusBadRequest {
			t.Errorf("status = %d, want 400 for %s", w.Code, body)
		}
		if len(h.cleanupCalls) != 0 {
			t.Errorf("nothing should be cloned before validation passes")
		}
	}
}

func TestExtract_MalformedJSON(t *testing.T) {
	h := newHarness(t)
	if w := h.post(`{"baseSha":`); w.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", w.Code)
	}
}

func TestExtract_CleanupAlwaysRuns(t *testing.T) {
	t.Run("on success", func(t *testing.T) {
		h := newHarness(t)
		h.postDefault()
		if !reflect.DeepEqual(h.cleanupCalls, []string{h.workDir}) {
			t.Errorf("cleanup calls = %v, want [%s]", h.cleanupCalls, h.workDir)
		}
	})
	t.Run("on a mid-pipeline failure", func(t *testing.T) {
		h := newHarness(t)
		h.srv.Deps.Diff = func(ctx context.Context, bin mx.Binary, b, hd, o string) (mx.DiffResult, error) {
			return mx.DiffResult{}, errors.New("boom")
		}
		h.postDefault()
		if len(h.cleanupCalls) != 1 {
			t.Errorf("cleanup calls = %v, want exactly one", h.cleanupCalls)
		}
	})
}

func TestExtract_CloneFailureIsBadGateway(t *testing.T) {
	h := newHarness(t)
	h.srv.Deps.CloneBoth = func(ctx context.Context, workRoot string, req gitops.CloneBothRequest) (gitops.CloneBothResult, error) {
		return gitops.CloneBothResult{}, errors.New("remote hung up")
	}
	w := h.postDefault()
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
	if len(h.cleanupCalls) != 0 {
		t.Errorf("nothing to clean up when the clone itself failed")
	}
}

func TestExtract_ShasReachCloneBoth(t *testing.T) {
	h := newHarness(t)
	var got gitops.CloneBothRequest
	inner := h.srv.Deps.CloneBoth
	h.srv.Deps.CloneBoth = func(ctx context.Context, workRoot string, req gitops.CloneBothRequest) (gitops.CloneBothResult, error) {
		got = req
		return inner(ctx, workRoot, req)
	}
	h.post(`{"repoUrl":"https://git.example/x.git","username":"pat","pat":"s3cret","baseSha":"aaaaaaa","headSha":"bbbbbbb"}`)

	want := gitops.CloneBothRequest{RepoURL: "https://git.example/x.git", Username: "pat", Pat: "s3cret", BaseSha: "aaaaaaa", HeadSha: "bbbbbbb"}
	if got != want {
		t.Errorf("CloneBoth request = %+v, want %+v", got, want)
	}
}

// ---------------------------------------------------------------------------
// Version handling
// ---------------------------------------------------------------------------

// A Projects$ProjectConversion unit persists in the model after a Studio Pro
// upgrade completes, so a healthy app carries one forever. This used to be a
// 422 gate and it rejected every commit of the real test app.
func TestExtract_ProjectConversionIsOnlyAWarning(t *testing.T) {
	for _, side := range []string{"base", "head", "both"} {
		t.Run(side, func(t *testing.T) {
			h := newHarness(t)
			var diffCalled bool
			h.srv.Deps.Diff = func(ctx context.Context, bin mx.Binary, b, hd, o string) (mx.DiffResult, error) {
				diffCalled = true
				return mx.DiffResult{}, nil
			}
			h.srv.Deps.PrepareMpr = func(ctx context.Context, bin mx.Binary, dir string) (string, mx.AnalyzeResult, error) {
				res := mx.AnalyzeResult{MendixVersion: "11.13.0"}
				if side == "both" || filepath.Base(dir) == side {
					res.HasProjectConversion = true
				}
				return filepath.Join(dir, "MERA.mpr"), res, nil
			}

			resp := decodeExtract(t, h.postDefault())
			if !diffCalled {
				t.Error("the diff must still run — presence of the unit proves nothing")
			}
			if !hasWarning(resp, "ProjectConversion") {
				t.Errorf("it should still be reported as a warning, got %v", resp.Warnings)
			}
			// One warning even when both sides carry it.
			var n int
			for _, warn := range resp.Warnings {
				if strings.Contains(warn, "ProjectConversion") {
					n++
				}
			}
			if n != 1 {
				t.Errorf("want 1 ProjectConversion warning, got %d: %v", n, resp.Warnings)
			}
		})
	}
}

// The authoritative signal: mx itself failing to parse the snapshot.
func TestExtract_UnparseableSnapshotIs422(t *testing.T) {
	realStderr := "Expected '$ID' as the first property of a storage object, but got 'Associations'."

	cases := map[string]error{
		"exit 129":          &mx.ErrDiffFailed{Stderr: realStderr},
		"undocumented code": &mx.ErrUnexpectedExitCode{ExitCode: 137, Stderr: realStderr},
	}
	for name, diffErr := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.srv.Deps.Diff = func(ctx context.Context, bin mx.Binary, b, hd, o string) (mx.DiffResult, error) {
				return mx.DiffResult{}, diffErr
			}
			w := h.postDefault()
			if w.Code != http.StatusUnprocessableEntity {
				t.Fatalf("status = %d, want 422; body = %s", w.Code, w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "migration") {
				t.Errorf("error should name the migration case: %s", w.Body.String())
			}
			if !strings.Contains(w.Body.String(), "11.13.0") {
				t.Errorf("error should carry the detected version: %s", w.Body.String())
			}
		})
	}
}

// An unrelated diff failure must not be mislabelled as a migration commit.
func TestExtract_OtherDiffFailureIsNotAMigration(t *testing.T) {
	h := newHarness(t)
	h.srv.Deps.Diff = func(ctx context.Context, bin mx.Binary, b, hd, o string) (mx.DiffResult, error) {
		return mx.DiffResult{}, &mx.ErrDiffFailed{Stderr: "some other catastrophe"}
	}
	w := h.postDefault()
	if w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
	if strings.Contains(w.Body.String(), "migration") {
		t.Errorf("must not claim migration: %s", w.Body.String())
	}
}

func TestExtract_UnsupportedVersionFailsLoudly(t *testing.T) {
	h := newHarness(t)
	h.srv.Deps.MxResolve = func(mxRoot, v string) (mx.Binary, error) {
		return mx.Binary{}, fmt.Errorf("no binary for %s", v)
	}
	w := h.postDefault()
	if w.Code != http.StatusUnprocessableEntity {
		t.Fatalf("status = %d, want 422", w.Code)
	}
	if !strings.Contains(w.Body.String(), "unsupportedMendixVersion") {
		t.Errorf("body should carry the contract's error name, got %s", w.Body.String())
	}
}

func TestExtract_NoBinariesIsServerError(t *testing.T) {
	h := newHarness(t)
	h.srv.Deps.MxHighest = func(mxRoot string) (mx.Binary, error) {
		return mx.Binary{}, errors.New("empty")
	}
	if w := h.postDefault(); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 — a missing binary is our misconfiguration", w.Code)
	}
}

// A relative MxRoot resolves against the SERVER's working directory, which the
// person reading the error cannot see. Naming both halves is what turns three
// repeat incidents into a message that explains itself.
func TestExtract_RelativeMxRootErrorNamesTheResolvedPath(t *testing.T) {
	h := newHarness(t)
	h.srv.MxRoot = "./.mx-binaries"
	h.srv.Deps.MxHighest = func(mxRoot string) (mx.Binary, error) {
		return mx.Binary{}, errors.New("no such file or directory")
	}
	// Decode rather than string-matching the raw body: JSON escapes the quotes
	// describeMxRoot emits, so `"./.mx-binaries"` appears on the wire as
	// \"./.mx-binaries\" and a naive Contains check fails on a correct message.
	var payload struct {
		Error string `json:"error"`
	}
	rec := h.postDefault()
	if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode error body: %v\n%s", err, rec.Body.String())
	}

	if !strings.Contains(payload.Error, `"./.mx-binaries"`) {
		t.Errorf("error should quote the configured value: %s", payload.Error)
	}
	if !strings.Contains(payload.Error, "resolved to") {
		t.Errorf("error should say where that actually points: %s", payload.Error)
	}
	cwd, _ := os.Getwd()
	if !strings.Contains(payload.Error, filepath.Join(cwd, ".mx-binaries")) {
		t.Errorf("resolved path should be the absolute one: %s", payload.Error)
	}
}

func TestDescribeMxRoot_AbsolutePathStaysPlain(t *testing.T) {
	// No point printing the same string twice when it is already absolute.
	got := describeMxRoot("/opt/mx")
	if got != `"/opt/mx"` {
		t.Errorf("describeMxRoot(/opt/mx) = %s, want it unadorned", got)
	}
}

func TestExtract_HeadVersionSelectsTheBinary(t *testing.T) {
	h := newHarness(t)
	h.srv.Deps.PrepareMpr = func(ctx context.Context, bin mx.Binary, dir string) (string, mx.AnalyzeResult, error) {
		v := "11.13.0"
		if filepath.Base(dir) == "base" {
			v = "11.12.0"
		}
		return filepath.Join(dir, "MERA.mpr"), mx.AnalyzeResult{MendixVersion: v}, nil
	}
	resp := decodeExtract(t, h.postDefault())

	if resp.MxVersion != "11.13.0" || resp.MendixVersion != "11.13.0" {
		t.Errorf("head version should win: mxVersion=%q mendixVersion=%q", resp.MxVersion, resp.MendixVersion)
	}
	if !hasWarning(resp, "base is Mendix 11.12.0 but head is 11.13.0") {
		t.Errorf("a version mismatch must be warned about, got %v", resp.Warnings)
	}
}

func TestExtract_UnsupportedMendixVersionFromDiffIs422(t *testing.T) {
	h := newHarness(t)
	h.srv.Deps.Diff = func(ctx context.Context, bin mx.Binary, b, hd, o string) (mx.DiffResult, error) {
		return mx.DiffResult{}, &mx.ErrUnsupportedMendixVersion{Stderr: "nope"}
	}
	if w := h.postDefault(); w.Code != http.StatusUnprocessableEntity {
		t.Errorf("status = %d, want 422", w.Code)
	}
}

func TestExtract_GenericDiffFailureIs500(t *testing.T) {
	h := newHarness(t)
	h.srv.Deps.Diff = func(ctx context.Context, bin mx.Binary, b, hd, o string) (mx.DiffResult, error) {
		return mx.DiffResult{}, &mx.ErrDiffFailed{Stderr: "boom"}
	}
	if w := h.postDefault(); w.Code != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500", w.Code)
	}
}

func TestExtract_ConflictsAreUsable(t *testing.T) {
	h := newHarness(t)
	h.srv.Deps.Diff = func(ctx context.Context, bin mx.Binary, b, hd, o string) (mx.DiffResult, error) {
		return mx.DiffResult{ConflictsFound: true}, nil
	}
	resp := decodeExtract(t, h.postDefault())
	if !hasWarning(resp, "conflicts") {
		t.Errorf("exit 2 should warn, not fail; warnings = %v", resp.Warnings)
	}
}

// ---------------------------------------------------------------------------
// The diff path proper
// ---------------------------------------------------------------------------

func TestExtract_EmptyDiffIsAValidEmptyResult(t *testing.T) {
	h := newHarness(t)
	rec := h.postDefault()
	resp := decodeExtract(t, rec)
	if len(resp.ChangeUnits) != 0 {
		t.Errorf("changeUnits = %v, want empty", resp.ChangeUnits)
	}
	// Must serialise as [] rather than null — a null here breaks strict clients.
	if !strings.Contains(rec.Body.String(), `"changeUnits":[]`) {
		t.Errorf("changeUnits should encode as [], got %s", rec.Body.String())
	}
	if len(h.resolveCalls) != 0 {
		t.Errorf("no ids to resolve means no dump-mpr call, got %v", h.resolveCalls)
	}
}

func TestExtract_AddedModifiedDeleted(t *testing.T) {
	h := newHarness(t)
	h.setDiff(
		mx.UnitDifference{Type: "Microflows$Microflow", ID: "id-add", Change: "Added", Raw: json.RawMessage(`{"change":"Added"}`)},
		mx.UnitDifference{Type: "Microflows$Microflow", ID: "id-mod", Change: "Modified", Raw: json.RawMessage(`{"change":"Modified"}`)},
		mx.UnitDifference{Type: "Microflows$Microflow", ID: "id-del", Change: "Deleted", Raw: json.RawMessage(`{"change":"Deleted"}`)},
	)
	h.setNames(
		map[string]mx.ResolvedUnit{ // base side
			"id-mod": mf("id-mod", "Sales.ACT_Modified"),
			"id-del": mf("id-del", "Sales.ACT_Deleted"),
		},
		map[string]mx.ResolvedUnit{ // head side
			"id-add": mf("id-add", "Sales.ACT_Added"),
			"id-mod": mf("id-mod", "Sales.ACT_Modified"),
		},
	)
	resp := decodeExtract(t, h.postDefault())

	if len(resp.ChangeUnits) != 3 {
		t.Fatalf("want 3 change units, got %d", len(resp.ChangeUnits))
	}
	// Order must mirror the diff's own order.
	if resp.ChangeUnits[0].ChangeKind != "Added" || resp.ChangeUnits[2].ChangeKind != "Deleted" {
		t.Errorf("change unit order does not mirror the diff: %+v", resp.ChangeUnits)
	}

	added := findUnit(t, resp, "Sales.ACT_Added")
	if added.BeforeMdl != "" {
		t.Errorf("an Added unit must have no beforeMdl, got %q", added.BeforeMdl)
	}
	if added.AfterMdl != "MDL(Sales.ACT_Added)" {
		t.Errorf("added.afterMdl = %q", added.AfterMdl)
	}

	deleted := findUnit(t, resp, "Sales.ACT_Deleted")
	if deleted.AfterMdl != "" {
		t.Errorf("a Deleted unit must have no afterMdl, got %q", deleted.AfterMdl)
	}
	if deleted.BeforeMdl != "MDL(Sales.ACT_Deleted)" {
		t.Errorf("deleted.beforeMdl = %q", deleted.BeforeMdl)
	}

	modified := findUnit(t, resp, "Sales.ACT_Modified")
	if modified.BeforeMdl == "" || modified.AfterMdl == "" {
		t.Errorf("a Modified unit needs both sides: %+v", modified)
	}
	if modified.Module != "Sales" {
		t.Errorf("module = %q, want Sales", modified.Module)
	}
	if string(modified.StructuralDelta) != `{"change":"Modified"}` {
		t.Errorf("structuralDelta = %s", modified.StructuralDelta)
	}
	if modified.TokenEstimate <= 0 {
		t.Errorf("tokenEstimate should be positive, got %d", modified.TokenEstimate)
	}
}

func TestExtract_DescribeUsesTheRightMprPerSide(t *testing.T) {
	h := newHarness(t)
	h.setDiff(mx.UnitDifference{Type: "Microflows$Microflow", ID: "id-mod", Change: "Modified"})
	h.setNames(
		map[string]mx.ResolvedUnit{"id-mod": mf("id-mod", "Sales.ACT_X")},
		map[string]mx.ResolvedUnit{"id-mod": mf("id-mod", "Sales.ACT_X")},
	)
	decodeExtract(t, h.postDefault())

	if len(h.describeCalls) != 2 {
		t.Fatalf("want 2 describe calls, got %v", h.describeCalls)
	}
	var sawBase, sawHead bool
	for _, c := range h.describeCalls {
		if c.MprPath == h.baseMpr {
			sawBase = true
		}
		if c.MprPath == h.headMpr {
			sawHead = true
		}
		if c.UnitType != "microflow" {
			t.Errorf("unit type = %q, want the mxcli singular form", c.UnitType)
		}
	}
	if !sawBase || !sawHead {
		t.Errorf("describe should hit both .mpr files, got %v", h.describeCalls)
	}
}

// A rename inside one commit is the case that justifies carrying two names
// all the way to the describe stage — the response has only one.
func TestExtract_RenameDescribesEachSideByItsOwnName(t *testing.T) {
	h := newHarness(t)
	h.setDiff(mx.UnitDifference{Type: "Microflows$Microflow", ID: "id-ren", Change: "Modified"})
	h.setNames(
		map[string]mx.ResolvedUnit{"id-ren": mf("id-ren", "Sales.ACT_OldName")},
		map[string]mx.ResolvedUnit{"id-ren": mf("id-ren", "Sales.ACT_NewName")},
	)
	resp := decodeExtract(t, h.postDefault())

	u := findUnit(t, resp, "Sales.ACT_NewName")
	if u.PreviousQualifiedName != "Sales.ACT_OldName" {
		t.Errorf("previousQualifiedName = %q, want Sales.ACT_OldName", u.PreviousQualifiedName)
	}
	if u.BeforeMdl != "MDL(Sales.ACT_OldName)" {
		t.Errorf("beforeMdl must come from the OLD name, got %q", u.BeforeMdl)
	}
	if u.AfterMdl != "MDL(Sales.ACT_NewName)" {
		t.Errorf("afterMdl must come from the NEW name, got %q", u.AfterMdl)
	}

	for _, c := range h.describeCalls {
		if c.MprPath == h.baseMpr && c.QualifiedName != "Sales.ACT_OldName" {
			t.Errorf("base describe used %q", c.QualifiedName)
		}
		if c.MprPath == h.headMpr && c.QualifiedName != "Sales.ACT_NewName" {
			t.Errorf("head describe used %q", c.QualifiedName)
		}
	}
}

func TestExtract_NoRenameLeavesPreviousNameEmpty(t *testing.T) {
	h := newHarness(t)
	h.setDiff(mx.UnitDifference{Type: "Microflows$Microflow", ID: "id", Change: "Modified"})
	h.setNames(
		map[string]mx.ResolvedUnit{"id": mf("id", "Sales.ACT_Same")},
		map[string]mx.ResolvedUnit{"id": mf("id", "Sales.ACT_Same")},
	)
	rec := h.postDefault()
	resp := decodeExtract(t, rec)
	if u := findUnit(t, resp, "Sales.ACT_Same"); u.PreviousQualifiedName != "" {
		t.Errorf("previousQualifiedName should be empty, got %q", u.PreviousQualifiedName)
	}
	if strings.Contains(rec.Body.String(), "previousQualifiedName") {
		t.Error("previousQualifiedName must be omitted entirely when there is no rename")
	}
}

func TestExtract_ResolveIsOncePerSideWithSortedTypes(t *testing.T) {
	h := newHarness(t)
	h.setDiff(
		mx.UnitDifference{Type: "Pages$Page", ID: "a", Change: "Modified"},
		mx.UnitDifference{Type: "Microflows$Microflow", ID: "b", Change: "Modified"},
		mx.UnitDifference{Type: "Pages$Page", ID: "c", Change: "Modified"},
	)
	h.setNames(map[string]mx.ResolvedUnit{}, map[string]mx.ResolvedUnit{})
	decodeExtract(t, h.postDefault())

	if len(h.resolveCalls) != 2 {
		t.Fatalf("want exactly one dump-mpr call per side, got %v", h.resolveCalls)
	}
	want := []string{"Microflows$Microflow", "Pages$Page"}
	for i, got := range h.resolveTypes {
		if !reflect.DeepEqual(got, want) {
			t.Errorf("call %d unit types = %v, want deduped and sorted %v", i, got, want)
		}
	}
}

func TestExtract_AddedUnitIsNotLookedUpInBase(t *testing.T) {
	h := newHarness(t)
	h.setDiff(mx.UnitDifference{Type: "Microflows$Microflow", ID: "only-added", Change: "Added"})
	h.setNames(map[string]mx.ResolvedUnit{}, map[string]mx.ResolvedUnit{"only-added": mf("only-added", "Sales.ACT_New")})
	decodeExtract(t, h.postDefault())

	for _, p := range h.resolveCalls {
		if p == h.baseMpr {
			t.Error("an all-Added diff has nothing to resolve on the base side")
		}
	}
}

// ---------------------------------------------------------------------------
// Degradation — partial success is the contract, per manual §1.4
// ---------------------------------------------------------------------------

func TestExtract_DescribeFailureDegradesToAWarning(t *testing.T) {
	h := newHarness(t)
	h.setDiff(
		mx.UnitDifference{Type: "Microflows$Microflow", ID: "good", Change: "Added"},
		mx.UnitDifference{Type: "Microflows$Microflow", ID: "bad", Change: "Added"},
	)
	h.setNames(nil, map[string]mx.ResolvedUnit{
		"good": mf("good", "Sales.ACT_Good"),
		"bad":  mf("bad", "Sales.ACT_Bad"),
	})
	h.srv.Deps.Describe = func(ctx context.Context, req mxcli.DescribeRequest) (string, error) {
		if req.QualifiedName == "Sales.ACT_Bad" {
			return "", errors.New("unsupported widget")
		}
		return "MDL(" + req.QualifiedName + ")", nil
	}

	resp := decodeExtract(t, h.postDefault())
	if len(resp.ChangeUnits) != 2 {
		t.Fatalf("both units must still be reported, got %d", len(resp.ChangeUnits))
	}
	if findUnit(t, resp, "Sales.ACT_Good").AfterMdl == "" {
		t.Error("the healthy unit must still render")
	}
	if findUnit(t, resp, "Sales.ACT_Bad").AfterMdl != "" {
		t.Error("the failed unit must have no MDL")
	}
	if !hasWarning(resp, "unsupported widget") {
		t.Errorf("failure must surface as a warning, got %v", resp.Warnings)
	}
}

func TestExtract_ResolveFailureDegrades(t *testing.T) {
	h := newHarness(t)
	h.setDiff(mx.UnitDifference{Type: "Microflows$Microflow", ID: "x", Change: "Modified"})
	h.srv.Deps.ResolveQualifiedNames = func(ctx context.Context, bin mx.Binary, mprPath string, unitTypes []string, wantIDs map[string]bool) (map[string]mx.ResolvedUnit, error) {
		return nil, errors.New("dump-mpr exploded")
	}
	resp := decodeExtract(t, h.postDefault())

	if len(resp.ChangeUnits) != 1 {
		t.Fatalf("the unit must still be reported without a name, got %d", len(resp.ChangeUnits))
	}
	if resp.ChangeUnits[0].UnitType != "Microflows$Microflow" || resp.ChangeUnits[0].ChangeKind != "Modified" {
		t.Errorf("type and changeKind survive name-resolution failure: %+v", resp.ChangeUnits[0])
	}
	if !hasWarning(resp, "dump-mpr exploded") {
		t.Errorf("warnings = %v", resp.Warnings)
	}
	if !hasWarning(resp, "could not resolve a qualified name") {
		t.Errorf("the unresolved unit needs its own warning, got %v", resp.Warnings)
	}
}

func TestExtract_UnrecognisedTypeWarnsOncePerTypeAndSkipsDescribe(t *testing.T) {
	h := newHarness(t)
	h.setDiff(
		mx.UnitDifference{Type: "Fictional$Widget", ID: "u1", Change: "Modified"},
		mx.UnitDifference{Type: "Fictional$Widget", ID: "u2", Change: "Modified"},
		mx.UnitDifference{Type: "Other$Thing", ID: "u3", Change: "Added"},
		mx.UnitDifference{Type: "Microflows$Microflow", ID: "m1", Change: "Added"},
	)
	names := map[string]mx.ResolvedUnit{
		"u1": {ID: "u1", QualifiedName: "Sales.One", Module: "Sales"},
		"u2": {ID: "u2", QualifiedName: "Other.Two", Module: "Other"},
		"u3": {ID: "u3", QualifiedName: "Sales.Three", Module: "Sales"},
		"m1": mf("m1", "Sales.ACT_New"),
	}
	h.setNames(names, names)
	resp := decodeExtract(t, h.postDefault())

	if len(resp.ChangeUnits) != 4 {
		t.Fatalf("undescribable units are still reported, got %d", len(resp.ChangeUnits))
	}
	if u := findUnit(t, resp, "Sales.One"); u.AfterMdl != "" || u.BeforeMdl != "" {
		t.Errorf("an unrecognised type must carry no MDL: %+v", u)
	}
	if u := findUnit(t, resp, "Sales.ACT_New"); u.AfterMdl == "" {
		t.Error("a mapped type in the same batch must still render")
	}

	// One warning per distinct type — 600 units of one unknown type must not
	// produce 600 warnings.
	var repeated int
	for _, warn := range resp.Warnings {
		if strings.Contains(warn, "Fictional$Widget") {
			repeated++
		}
	}
	if repeated != 1 {
		t.Errorf("want exactly 1 warning for the repeated type, got %d: %v", repeated, resp.Warnings)
	}
	if !hasWarning(resp, "Other$Thing") {
		t.Errorf("each distinct unknown type needs its own warning, got %v", resp.Warnings)
	}

	// Only the microflow should have been described at all.
	if len(h.describeCalls) != 1 || h.describeCalls[0].QualifiedName != "Sales.ACT_New" {
		t.Errorf("describe calls = %v, want only the microflow", h.describeCalls)
	}
}

// A type that legitimately has no MDL must not clutter the warning list —
// otherwise every commit touching a folder or the domain model reads as though
// something went wrong.
func TestExtract_KnownNotDescribableTypesAreSilent(t *testing.T) {
	h := newHarness(t)
	var diffs []mx.UnitDifference
	names := map[string]mx.ResolvedUnit{}
	for i, typ := range []string{
		"Projects$Module", "Projects$Folder", "Projects$ModuleSettings",
		"DomainModels$DomainModel", "Security$ModuleSecurity",
	} {
		id := fmt.Sprintf("id-%d", i)
		diffs = append(diffs, mx.UnitDifference{Type: typ, ID: id, Change: "Modified"})
		names[id] = mx.ResolvedUnit{ID: id, QualifiedName: fmt.Sprintf("Sales.N%d", i), Module: "Sales", QualifiedNameSynthesized: true}
	}
	h.setDiff(diffs...)
	h.setNames(names, names)
	resp := decodeExtract(t, h.postDefault())

	if len(resp.ChangeUnits) != 5 {
		t.Fatalf("all five must still be reported, got %d", len(resp.ChangeUnits))
	}
	if len(resp.Warnings) != 0 {
		t.Errorf("expected no-MDL types must not warn, got %v", resp.Warnings)
	}
	if len(h.describeCalls) != 0 {
		t.Errorf("none of these should be described, got %v", h.describeCalls)
	}
	// The synthesized flag still has to reach the response so a reviewer can
	// see the name was inferred from the containment chain.
	if !resp.ChangeUnits[0].QualifiedNameSynthesized {
		t.Error("qualifiedNameSynthesized should travel through to the response")
	}
}

// Guards the two tables against each other and against typos: every key must
// look like a real metamodel type, and no key may appear in both.
func TestTypeTablesAreCoherent(t *testing.T) {
	valid := map[string]bool{}
	for _, ut := range []string{
		"module", "entity", "externalentity", "association", "enumeration", "constant",
		"microflow", "nanoflow", "workflow", "page", "snippet", "buildingblock", "layout",
		"javaaction", "jsonstructure", "importmapping", "exportmapping", "restclient",
		"odataclient", "odataservice", "imagecollection", "menu", "queue", "scheduledevent",
		"regularexpression", "businesseventservice", "databaseconnection", "agent", "aimodel",
		"knowledgebase", "consumedmcpservice", "datatransformer", "modulerole", "userrole",
		"projectsecurity", "settings", "demouser", "navigation", "systemoverview",
	} {
		valid[ut] = true
	}

	for k, v := range diffTypeToMxcli {
		if !strings.Contains(k, "$") {
			t.Errorf("diffTypeToMxcli key %q is not a Namespace$Type string", k)
		}
		if !valid[v] {
			t.Errorf("diffTypeToMxcli[%q] = %q, which is not in `mxcli describe --help`", k, v)
		}
		if _, dup := knownNotDescribable[k]; dup {
			t.Errorf("%q is in both diffTypeToMxcli and knownNotDescribable", k)
		}
	}
	for k, reason := range knownNotDescribable {
		if !strings.Contains(k, "$") {
			t.Errorf("knownNotDescribable key %q is not a Namespace$Type string", k)
		}
		if reason == "" {
			t.Errorf("knownNotDescribable[%q] needs a reason", k)
		}
	}

	// Every top-level unit type seen in the real dump fixture must be
	// accounted for in one table or the other — that fixture is the only
	// ground truth available, so an unclassified entry means a gap we know
	// about and have not decided on.
	for _, typ := range []string{
		"Projects$Module", "Projects$Folder", "Pages$Page", "Pages$Snippet",
		"Constants$Constant", "ExportMappings$ExportMapping", "JavaActions$JavaAction",
		"Enumerations$Enumeration", "Microflows$Microflow", "JsonStructures$JsonStructure",
		"ImportMappings$ImportMapping", "DomainModels$DomainModel",
		"Projects$ModuleSettings", "Security$ModuleSecurity",
	} {
		_, mapped := diffTypeToMxcli[typ]
		_, known := knownNotDescribable[typ]
		if !mapped && !known {
			t.Errorf("%q was seen in real dump-mpr output but is classified nowhere", typ)
		}
	}
}

// ---------------------------------------------------------------------------
// Step 10 — textDiffs
// ---------------------------------------------------------------------------

func TestExtract_TextDiffsReachTheResponse(t *testing.T) {
	h := newHarness(t)
	resp := decodeExtract(t, h.postDefault())

	if len(resp.TextDiffs) != 1 || resp.TextDiffs[0].Path != "javasource/sales/Foo.java" {
		t.Fatalf("textDiffs = %+v", resp.TextDiffs)
	}
	// Must run against the bare repo with the request's own SHAs — not the
	// worktree directories, and not some rev git would have to resolve.
	want := [3]string{filepath.Join(h.workDir, "repo.git"), "aaaaaaa", "bbbbbbb"}
	if len(h.textDiffCalls) != 1 || h.textDiffCalls[0] != want {
		t.Errorf("TextDiffs called with %v, want [%v]", h.textDiffCalls, want)
	}
}

func TestExtract_TextDiffFailureIsOnlyAWarning(t *testing.T) {
	h := newHarness(t)
	h.srv.Deps.TextDiffs = func(ctx context.Context, repoDir, b, hd string, p []string) ([]gitops.TextDiff, error) {
		return nil, errors.New("git exploded")
	}
	// The mx half of the extraction is independent of the text half; losing
	// one must not lose the other.
	h.setDiff(mx.UnitDifference{Type: "Microflows$Microflow", ID: "m", Change: "Added"})
	h.setNames(nil, map[string]mx.ResolvedUnit{"m": mf("m", "Sales.ACT_X")})

	resp := decodeExtract(t, h.postDefault())
	if !hasWarning(resp, "git exploded") {
		t.Errorf("warnings = %v", resp.Warnings)
	}
	if findUnit(t, resp, "Sales.ACT_X").AfterMdl == "" {
		t.Error("a text-diff failure must not cost the model-side results")
	}
}

func TestExtract_EscapeHatchSkipsTextDiffs(t *testing.T) {
	h := newHarness(t)
	h.post(`{"repoUrl":"https://git.example/x.git","baseSha":"aaaaaaa","headSha":"bbbbbbb","modules":["Sales"]}`)
	if len(h.textDiffCalls) != 0 {
		t.Errorf("the legacy path has no textDiffs field to fill, got %v", h.textDiffCalls)
	}
}

func TestExtract_MxcliVersionFailureIsOnlyAWarning(t *testing.T) {
	h := newHarness(t)
	h.srv.Deps.MxcliVersion = func(ctx context.Context) (string, error) { return "", errors.New("not on PATH") }
	resp := decodeExtract(t, h.postDefault())
	if !hasWarning(resp, "mxcli version") {
		t.Errorf("warnings = %v", resp.Warnings)
	}
}

// ---------------------------------------------------------------------------
// The escape hatch must be untouched by Stage 8
// ---------------------------------------------------------------------------

func TestExtract_EscapeHatchReturnsTheOldShape(t *testing.T) {
	h := newHarness(t)
	var diffCalled bool
	h.srv.Deps.Diff = func(ctx context.Context, bin mx.Binary, b, hd, o string) (mx.DiffResult, error) {
		diffCalled = true
		return mx.DiffResult{}, nil
	}
	w := h.post(`{"repoUrl":"https://git.example/x.git","baseSha":"aaaaaaa","headSha":"bbbbbbb",
	              "units":[{"unitType":"microflow","qualifiedName":"Sales.ACT_One"}]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	if diffCalled {
		t.Error("the escape hatch must skip the diff path entirely")
	}

	body := w.Body.String()
	for _, absent := range []string{"changeUnits", "mendixVersion", "mxVersion"} {
		if strings.Contains(body, absent) {
			t.Errorf("legacy response leaked the new field %q: %s", absent, body)
		}
	}

	var legacy legacyExtractResponse
	if err := json.Unmarshal(w.Body.Bytes(), &legacy); err != nil {
		t.Fatal(err)
	}
	if len(legacy.Units) != 1 || legacy.Units[0].Mdl != "MDL(Sales.ACT_One)" {
		t.Errorf("legacy units = %+v", legacy.Units)
	}
	// Naive enumeration runs against HEAD, the only defensible reading now
	// that the request carries two commits.
	if legacy.MprPath != h.headMpr {
		t.Errorf("mprPath = %q, want the head worktree's %q", legacy.MprPath, h.headMpr)
	}
}

func TestExtract_EscapeHatchModulesEnumerates(t *testing.T) {
	h := newHarness(t)
	w := h.post(`{"repoUrl":"https://git.example/x.git","baseSha":"aaaaaaa","headSha":"bbbbbbb","modules":["Sales","Admin"]}`)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d: %s", w.Code, w.Body.String())
	}
	// One ListUnits call per (type, module) pair — unchanged from Stage 5.
	if want := len(mxcli.DefaultUnitTypes) * 2; h.listUnitsCalls != want {
		t.Errorf("ListUnits calls = %d, want %d", h.listUnitsCalls, want)
	}
}

func TestExtract_EscapeHatchEnumerationFailureIsBadGateway(t *testing.T) {
	h := newHarness(t)
	h.srv.Deps.ListUnits = func(ctx context.Context, mprPath, unitType, module string) ([]mxcli.UnitSummary, error) {
		return nil, errors.New("mxcli died")
	}
	w := h.post(`{"repoUrl":"https://git.example/x.git","baseSha":"aaaaaaa","headSha":"bbbbbbb","modules":["Sales"]}`)
	if w.Code != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", w.Code)
	}
}

// ---------------------------------------------------------------------------
// Units
// ---------------------------------------------------------------------------

func TestTokenEstimate(t *testing.T) {
	cases := []struct {
		parts []string
		want  int
	}{
		{nil, 0},
		{[]string{""}, 0},
		{[]string{strings.Repeat("a", 36)}, 10},
		{[]string{strings.Repeat("a", 18), strings.Repeat("b", 18)}, 10},
		{[]string{strings.Repeat("a", 3)}, 0}, // truncates toward zero
	}
	for _, c := range cases {
		if got := tokenEstimate(c.parts...); got != c.want {
			t.Errorf("tokenEstimate(%d chars) = %d, want %d", len(strings.Join(c.parts, "")), got, c.want)
		}
	}
}

func TestDistinctTypes(t *testing.T) {
	got := distinctTypes([]mx.UnitDifference{
		{Type: "Pages$Page"}, {Type: "Microflows$Microflow"}, {Type: "Pages$Page"}, {Type: ""},
	})
	want := []string{"Microflows$Microflow", "Pages$Page"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("distinctTypes = %v, want %v", got, want)
	}
}

func TestDepsWithDefaults_FillsEveryNil(t *testing.T) {
	// A nil dependency would panic at call time rather than fall back, so
	// this guards the seam itself — add a field to Deps and forget the
	// withDefaults line, and this fails.
	d := Deps{}.withDefaults()
	v := reflect.ValueOf(d)
	for i := 0; i < v.NumField(); i++ {
		if v.Field(i).IsNil() {
			t.Errorf("Deps.%s was left nil by withDefaults", v.Type().Field(i).Name)
		}
	}
}

func TestDepsWithDefaults_KeepsOverrides(t *testing.T) {
	sentinel := errors.New("mine")
	d := Deps{Cleanup: func(string) error { return sentinel }}.withDefaults()
	if !errors.Is(d.Cleanup(""), sentinel) {
		t.Error("withDefaults must not clobber an explicitly set dependency")
	}
}
