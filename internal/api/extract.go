// internal/api/extract.go
package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sync"

	"mera-extractor/internal/gitops"
	"mera-extractor/internal/mxcli"
	//"mera-extractor/internal/mx"
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

	unitResults, warnings := describeAll(r.Context(), clone.MprPath, units)

	resp := extractResponse{
		MprPath:  clone.MprPath,
		Units:    unitResults,
		Warnings: warnings,
	}
	writeJSON(w, http.StatusOK, resp)
}

// describeOutcome pairs a unit's rendered result with its warning text (if
// any), so both can travel together through the same indexed slot.
type describeOutcome struct {
	result  unitResult
	warning string // "" if the describe succeeded
}

// describeAll renders every unit in units concurrently.
// Each goroutine writes only to its own index in outcomes — no mutex
// needed. Output order still matches input order regardless of completion order.

func describeAll(ctx context.Context, mprPath string, units []unitRequest) ([]unitResult, []string) {
	if len(units) == 0 {
		return nil, nil
	}

	outcomes := make([]describeOutcome, len(units))

	var wg sync.WaitGroup
	wg.Add(len(units))
	for i, u := range units {
		go func(i int, u unitRequest) {
			defer wg.Done()
			mdl, err := mxcli.Describe(ctx, mxcli.DescribeRequest{
				MprPath: mprPath, UnitType: u.UnitType, QualifiedName: u.QualifiedName,
			})
			res := unitResult{QualifiedName: u.QualifiedName}
			var warn string
			if err != nil {
				// One bad unit must never fail the whole extraction —
				// manual §1.4's core design rule, unchanged here.
				res.Warning = err.Error()
				warn = u.QualifiedName + ": " + err.Error()
			} else {
				res.Mdl = mdl
			}
			outcomes[i] = describeOutcome{result: res, warning: warn}
		}(i, u)
	}
	wg.Wait()

	results := make([]unitResult, len(outcomes))
	var warnings []string
	for i, o := range outcomes {
		results[i] = o.result
		if o.warning != "" {
			warnings = append(warnings, o.warning)
		}
	}
	return results, warnings
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