// internal/api/extract.go
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"mera-extractor/internal/gitops"
	"mera-extractor/internal/mxcli"
)

type unitRequest struct {
	UnitType      string `json:"unitType"`
	QualifiedName string `json:"qualifiedName"`
}

type extractRequest struct {
	RepoURL  string        `json:"repoUrl"`
	Username string        `json:"username"`
	Pat      string        `json:"pat"`
	Sha      string        `json:"sha"`
	Units    []unitRequest `json:"units"`   // optional — explicit list, takes precedence over Modules
	Modules  []string      `json:"modules"` // optional — scopes auto-enumeration; empty means "every module"
}

type unitResult struct {
	QualifiedName string `json:"qualifiedName"`
	Mdl           string `json:"mdl,omitempty"`
	Warning       string `json:"warning,omitempty"`
}

type extractResponse struct {
	MprPath  string       `json:"mprPath"`
	Units    []unitResult `json:"units"`
	Warnings []string     `json:"warnings,omitempty"`
}

func (s *Server) handleExtract(w http.ResponseWriter, r *http.Request) {
	var req extractRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}

	clone, err := gitops.Clone(r.Context(), s.WorkRoot, gitops.CloneRequest{
		RepoURL: req.RepoURL, Username: req.Username, Pat: req.Pat, Sha: req.Sha,
	})
	if err != nil {
		respondError(w, http.StatusBadGateway, err)
		return
	}
	// Unlike /clone, this whole lifecycle fits in one request — clone,
	// describe everything, respond — so cleaning up via defer is correct
	// and keeps the container's disk from filling up over many requests.
	defer gitops.Cleanup(clone.WorkDir)

	units := req.Units
	if len(units) == 0 {
		enumerated, err := enumerate(r.Context(), clone.MprPath, req.Modules)
		if err != nil {
			respondError(w, http.StatusBadGateway, err)
			return
		}
		units = enumerated
	}

	resp := extractResponse{MprPath: clone.MprPath}

	// Sequential for now — one unit at a time. Once you're describing
	// hundreds of units in a real change set, this is the first place to
	// add a worker pool (manual §1.5 uses ~8 concurrent workers). Get it
	// correct sequentially before making it concurrent — a bug is much
	// easier to find in a loop than in eight goroutines.
	for _, u := range units {
		mdl, err := mxcli.Describe(r.Context(), mxcli.DescribeRequest{
			MprPath: clone.MprPath, UnitType: u.UnitType, QualifiedName: u.QualifiedName,
		})
		result := unitResult{QualifiedName: u.QualifiedName}
		if err != nil {
			// One bad unit must never fail the whole extraction —
			// manual §1.4's core design rule. Record it and move on.
			result.Warning = err.Error()
			resp.Warnings = append(resp.Warnings, u.QualifiedName+": "+err.Error())
		} else {
			result.Mdl = mdl
		}
		resp.Units = append(resp.Units, result)
	}

	writeJSON(w, http.StatusOK, resp)
}

// enumerate lists units across mxcli.DefaultUnitTypes — the "naive
// extract" path: render the whole app (or a chosen subset of it), since
// there's no mx diff yet to know what actually changed.
//
// modules is optional. Empty means every module — one unscoped ListUnits
// call per type. A non-empty list scopes each type to just those modules,
// which costs one ListUnits call per (type, module) pair but returns far
// fewer units downstream, since every extra unit here is an extra
// `mxcli describe` subprocess spawn later in the same request.
func enumerate(ctx context.Context, mprPath string, modules []string) ([]unitRequest, error) {
	scopes := modules
	if len(scopes) == 0 {
		scopes = []string{""} // one unscoped pass per type — every module
	}

	var all []unitRequest
	for _, unitType := range mxcli.DefaultUnitTypes {
		for _, module := range scopes {
			units, err := mxcli.ListUnits(ctx, mprPath, unitType, module)
			if err != nil {
				return nil, fmt.Errorf("list %s (module=%q): %w", unitType, module, err)
			}
			for _, u := range units {
				all = append(all, unitRequest{UnitType: unitType, QualifiedName: u.QualifiedName})
			}
		}
	}
	return all, nil
}