// internal/api/server.go
package api

import "net/http"

type Server struct {
	WorkRoot string
	MxRoot   string
	Deps     Deps // ← new. Zero value means "use the real tools".
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
