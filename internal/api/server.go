// internal/api/server.go
package api

import "net/http"

type Server struct {
	WorkRoot string // where clones get created; see gitops.Clone
}

func NewServer(workRoot string) *Server {
	return &Server{WorkRoot: workRoot}
}

func (s *Server) Routes() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", s.handleHealth)
	mux.HandleFunc("POST /describe", s.handleDescribe)
	mux.HandleFunc("POST /clone", s.handleClone)
	mux.HandleFunc("POST /extract", s.handleExtract)
	return mux
}