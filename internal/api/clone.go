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
		respondError(w, r, http.StatusBadRequest, err)
		return
	}

	result, err := gitops.Clone(r.Context(), s.WorkRoot, gitops.CloneRequest{
		RepoURL: req.RepoURL, Username: req.Username, Pat: req.Pat, Sha: req.Sha,
	})
	if err != nil {
		respondError(w, r, http.StatusBadGateway, err) // upstream (git) failed, not our code
		return
	}
	// This clone is NOT cleaned up here — the whole point of a standalone
	// /clone is that the caller uses WorkDir afterward (e.g. against
	// /describe). It is no longer an unbounded leak: it lands under the
	// per-instance workspace dir (s.WorkRoot is workspace.Manager.Dir()), so
	// the janitor reclaims it once it has been idle past MERA_WORKSPACE_TTL
	// (default 30m). A caller that needs it to live longer is asking for the
	// leased-workspace feature in manual §1.8, which is still unbuilt.
	writeJSON(w, http.StatusOK, result)
}
