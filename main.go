// main.go
package main

import (
	"log"
	"net/http"
	"os"

	"mera-extractor/internal/api"
)

func main() {
	workRoot := os.Getenv("MERA_WORK_ROOT")
	if workRoot == "" {
		workRoot = os.TempDir()
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080"
	}

	srv := api.NewServer(workRoot)

	addr := ":" + port
	log.Printf("extractor listening on %s (workRoot=%s)", addr, workRoot)
	log.Fatal(http.ListenAndServe(addr, srv.Routes()))
}