// main.go
package main

import (
	"log"
	"net/http"
	"os"
	"strconv"

	"mera-extractor/internal/api"
	"mera-extractor/internal/mxcli"
)

func main() {
	workRoot := os.Getenv("MERA_WORK_ROOT")
	if workRoot == "" {
		workRoot = os.TempDir()
	}

	port := os.Getenv("PORT")
	if port == "" {
		port = "8080" // matches EXPOSE 8080 in the Dockerfile and your local curl tests
	}

	if v := os.Getenv("MERA_MXCLI_CONCURRENCY"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			mxcli.SetMaxConcurrent(n)
		} else {
			log.Printf("MERA_MXCLI_CONCURRENCY=%q invalid, keeping default (runtime.NumCPU())", v)
		}
	}

	srv := api.NewServer(workRoot)

	addr := ":" + port
	log.Printf("extractor listening on %s (workRoot=%s)", addr, workRoot)
	log.Fatal(http.ListenAndServe(addr, srv.Routes()))
}