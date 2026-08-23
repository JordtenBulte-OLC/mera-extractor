// internal/api/health.go
package api

import (
	"net/http"
	"path/filepath"
)

// healthResponse is a capability statement, not just a liveness ping: it tells
// a caller which Mendix versions this container can actually diff before they
// send a request that would fail on one.
type healthResponse struct {
	Status string `json:"status"` // "ok" | "degraded"
	Mxcli  string `json:"mxcli,omitempty"`

	// MxRoot is the RESOLVED absolute path, not the configured value. That
	// variable has caused three separate incidents by pointing somewhere other
	// than the reader assumed (see design notes §2), and surfacing it here
	// means a misconfigured root is visible from /health instead of only from
	// a failed /extract twenty seconds later.
	MxRoot string `json:"mxRoot,omitempty"`

	// MxVersions is what mx.ListVersions found on disk — never what
	// mx-versions.txt declares. The manifest is a build input and can drift
	// from the image; this endpoint must describe reality. Always encoded,
	// including as [], so a caller can distinguish "none" from "field absent".
	MxVersions []string `json:"mxVersions"`

	Warnings []string `json:"warnings,omitempty"`
}

// handleHealth always answers 200, even when degraded.
//
// If this is ever wired to a LIVENESS probe, failing it on a missing binary
// would restart-loop the container over a misconfiguration no restart can
// fix. A caller that wants readiness semantics should check `status` and
// `mxVersions` rather than the HTTP code.
func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	d := s.Deps.withDefaults()
	resp := healthResponse{Status: "ok", MxVersions: []string{}}

	if abs, err := filepath.Abs(s.MxRoot); err == nil {
		resp.MxRoot = abs
	} else {
		resp.MxRoot = s.MxRoot
	}

	version, err := d.MxcliVersion(r.Context())
	if err != nil {
		resp.Status = "degraded"
		resp.Warnings = append(resp.Warnings, "mxcli unavailable: "+err.Error())
	} else {
		resp.Mxcli = version
	}

	// Note this shares mx.ListVersions with Resolve and Highest, so /health
	// cannot advertise a version that /extract would then refuse.
	installed, err := d.MxListVersions(s.MxRoot)
	switch {
	case err != nil:
		// An unreadable root — almost always a bad MERA_MX_ROOT.
		resp.Status = "degraded"
		resp.Warnings = append(resp.Warnings, err.Error())
	case len(installed) == 0:
		// Readable but empty is a different fault from unreadable, and worth
		// saying so: the path is right, the image is wrong.
		resp.Status = "degraded"
		resp.Warnings = append(resp.Warnings,
			"no mx binaries installed; /extract cannot run")
	default:
		for _, b := range installed {
			resp.MxVersions = append(resp.MxVersions, b.Version)
		}
	}

	writeJSON(w, http.StatusOK, resp)
}
