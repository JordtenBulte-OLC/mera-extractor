// internal/api/clone.go
package api

import (
	"encoding/json"
	"net/http"

	"mera-extractor/internal/gitops"
)

type cloneRequest struct {
	RepoURL  string `json:"repoUrl"`
	Username string `json:"username"`
	Pat      string `json:"pat"`
	Sha      string `json:"sha"`
}

func (s *Server) handleClone(w http.ResponseWriter, r *http.Request) {
	var req cloneRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		respondError(w, http.StatusBadRequest, err)
		return
	}

	result, err := gitops.Clone(r.Context(), s.WorkRoot, gitops.CloneRequest{
		RepoURL: req.RepoURL, Username: req.Username, Pat: req.Pat, Sha: req.Sha,
	})
	if err != nil {
		respondError(w, http.StatusBadGateway, err) // upstream (git) failed, not our code
		return
	}
	// Note: this clone is NOT cleaned up here — the whole point of a
	// standalone /clone is that the caller uses WorkDir afterward (e.g.
	// against /describe). Nothing currently reaps it. That's a known gap —
	// see "What's still open" at the end of this guide.
	writeJSON(w, http.StatusOK, result)
}