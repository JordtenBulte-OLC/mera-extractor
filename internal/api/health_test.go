// internal/api/health_test.go
package api

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"mera-extractor/internal/mx"
)

func getHealth(t *testing.T, srv *Server) (*httptest.ResponseRecorder, healthResponse) {
	t.Helper()
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.handleHealth(w, r)

	var resp healthResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode: %v\n%s", err, w.Body.String())
	}
	return w, resp
}

func healthServer(mxRoot string, versions []mx.Binary, listErr error, mxcliVer string, mxcliErr error) *Server {
	return &Server{
		MxRoot: mxRoot,
		Deps: Deps{
			MxListVersions: func(string) ([]mx.Binary, error) { return versions, listErr },
			MxcliVersion:   func(context.Context) (string, error) { return mxcliVer, mxcliErr },
		},
	}
}

func TestHealth_ReportsInstalledVersions(t *testing.T) {
	srv := healthServer("/opt/mx", []mx.Binary{
		{Version: "11.12.0", Path: "/opt/mx/11.12.0/modeler/mx"},
		{Version: "11.13.0", Path: "/opt/mx/11.13.0/modeler/mx"},
	}, nil, "mxcli version v0.19.0", nil)

	w, resp := getHealth(t, srv)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	if resp.Status != "ok" {
		t.Errorf("status = %q, want ok", resp.Status)
	}
	if got := strings.Join(resp.MxVersions, ","); got != "11.12.0,11.13.0" {
		t.Errorf("mxVersions = %v", resp.MxVersions)
	}
	if resp.Mxcli == "" {
		t.Error("mxcli version missing")
	}
	if len(resp.Warnings) != 0 {
		t.Errorf("healthy response should carry no warnings: %v", resp.Warnings)
	}
	// Order comes from ListVersions, which sorts numerically — /health must
	// not re-sort it as strings.
	if resp.MxVersions[1] != "11.13.0" {
		t.Errorf("version order not preserved: %v", resp.MxVersions)
	}
}

// A misconfigured MERA_MX_ROOT has caused three incidents. Surfacing the
// resolved absolute path here means it is visible from /health rather than
// only from a failed /extract twenty seconds later.
func TestHealth_ReportsResolvedMxRoot(t *testing.T) {
	srv := healthServer("./.mx-binaries", []mx.Binary{{Version: "11.13.0"}}, nil, "v0.19.0", nil)
	_, resp := getHealth(t, srv)

	if !filepath.IsAbs(resp.MxRoot) {
		t.Errorf("mxRoot = %q, want an absolute path", resp.MxRoot)
	}
	if !strings.HasSuffix(resp.MxRoot, ".mx-binaries") {
		t.Errorf("mxRoot = %q", resp.MxRoot)
	}
}

func TestHealth_DegradedWhenRootUnreadable(t *testing.T) {
	srv := healthServer("/nope", nil, errors.New(`mx: reading "/nope": no such file or directory`), "v0.19.0", nil)

	w, resp := getHealth(t, srv)
	// Still 200: if this is ever a liveness probe, failing it would
	// restart-loop the container over a misconfiguration a restart cannot fix.
	if w.Code != http.StatusOK {
		t.Errorf("status code = %d, want 200 even when degraded", w.Code)
	}
	if resp.Status != "degraded" {
		t.Errorf("status = %q, want degraded", resp.Status)
	}
	if len(resp.MxVersions) != 0 {
		t.Errorf("mxVersions = %v, want empty", resp.MxVersions)
	}
	if !hasHealthWarning(resp, "no such file") {
		t.Errorf("warnings should carry the underlying error: %v", resp.Warnings)
	}
}

// Readable-but-empty is a different fault from unreadable — the path is right
// and the image is wrong — so the two must not produce the same message.
func TestHealth_DegradedWhenNoBinariesInstalled(t *testing.T) {
	srv := healthServer("/opt/mx", nil, nil, "v0.19.0", nil)

	_, resp := getHealth(t, srv)
	if resp.Status != "degraded" {
		t.Errorf("status = %q, want degraded", resp.Status)
	}
	if !hasHealthWarning(resp, "no mx binaries installed") {
		t.Errorf("warnings = %v", resp.Warnings)
	}
	if hasHealthWarning(resp, "reading") {
		t.Errorf("an empty root must not be reported as unreadable: %v", resp.Warnings)
	}
}

func TestHealth_DegradedWhenMxcliUnavailable(t *testing.T) {
	srv := healthServer("/opt/mx", []mx.Binary{{Version: "11.13.0"}}, nil, "", errors.New("not on PATH"))

	_, resp := getHealth(t, srv)
	if resp.Status != "degraded" {
		t.Errorf("status = %q, want degraded", resp.Status)
	}
	if !hasHealthWarning(resp, "mxcli unavailable") {
		t.Errorf("warnings = %v", resp.Warnings)
	}
	// The mx side is independent and must still be reported.
	if len(resp.MxVersions) != 1 {
		t.Errorf("mx versions should survive an mxcli failure: %v", resp.MxVersions)
	}
}

func TestHealth_BothBrokenReportsBoth(t *testing.T) {
	srv := healthServer("/nope", nil, errors.New("boom"), "", errors.New("gone"))
	_, resp := getHealth(t, srv)

	if len(resp.Warnings) != 2 {
		t.Errorf("want a warning per fault, got %v", resp.Warnings)
	}
}

// A strict client distinguishing "no versions" from "field absent" needs [],
// not null. omitempty on a slice would drop it in exactly the degraded case
// where it matters most.
func TestHealth_EmptyVersionsEncodeAsArray(t *testing.T) {
	srv := healthServer("/opt/mx", nil, nil, "v0.19.0", nil)
	r := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	srv.handleHealth(w, r)

	if !strings.Contains(w.Body.String(), `"mxVersions":[]`) {
		t.Errorf("want mxVersions:[], got %s", w.Body.String())
	}
}

// /health must work on a Server built by NewServer, with no Deps overrides —
// otherwise the seam has quietly become mandatory.
func TestHealth_ZeroDepsUsesRealImplementations(t *testing.T) {
	srv := NewServer(t.TempDir(), t.TempDir())
	w, resp := getHealth(t, srv)

	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	// An empty (real) mx root and no mxcli on PATH in CI — degraded is the
	// correct answer, and the point is that it answered at all.
	if resp.Status == "" {
		t.Error("status missing")
	}
	if resp.MxVersions == nil {
		t.Error("mxVersions should be non-nil even with real deps")
	}
}

func hasHealthWarning(resp healthResponse, substr string) bool {
	for _, w := range resp.Warnings {
		if strings.Contains(w, substr) {
			return true
		}
	}
	return false
}
