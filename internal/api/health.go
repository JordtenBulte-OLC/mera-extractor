// internal/api/health.go
package api

import (
	"net/http"

	"mera-extractor/internal/mxcli"
)

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	version, err := mxcli.Version(r.Context())
	resp := map[string]string{"status": "ok", "mxcli": version}
	if err != nil {
		resp["status"] = "degraded"
		resp["error"] = err.Error()
	}
	writeJSON(w, http.StatusOK, resp)
}	