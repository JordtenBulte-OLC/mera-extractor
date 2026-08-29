// internal/api/server.go
package api

import (
	"net/http"

	"mera-extractor/internal/workspace"
)

type Server struct {
	WorkRoot string
	MxRoot   string
	Deps     Deps // ← new. Zero value means "use the real tools".

	// Workspace, when set, heartbeats each in-flight /extract clone dir so
	// the workspace janitor never reaps one a slow request is still using.
	// Nil is valid — Manager.Track is nil-safe — so every existing
	// &Server{...} in the tests keeps working with no change.
	Workspace *workspace.Manager
}

func NewServer(workRoot, mxRoot string) *Server {
	return &Server{WorkRoot: workRoot, MxRoot: mxRoot}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /describe", s.handleDescribe)
	mux.HandleFunc("POST /clone", s.handleClone)
	mux.HandleFunc("POST /extract", s.handleExtract)
	return mux
}
