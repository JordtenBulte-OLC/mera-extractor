// main.go
package main

import (
	"io"
	"log"
	"net/http"
	"os"
	"strconv"

	"github.com/joho/godotenv"

	"mera-extractor/internal/api"
	"mera-extractor/internal/mx"
	"mera-extractor/internal/mxcli"
)

func main() {
	if err := godotenv.Load(); err != nil {
		log.Println("no .env file found, relying on real environment")
	}

	workRoot := os.Getenv("MERA_WORK_ROOT")
	if workRoot == "" {
		workRoot = os.TempDir()
	}

	mxRoot := os.Getenv("MERA_MX_ROOT")
	if mxRoot == "" {
		mxRoot = "/opt/mx"
	}

	if _, err := mx.Highest(mxRoot); err != nil {
		log.Printf("warning: no usable mx binary found under %s: %v", mxRoot, err)
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

	if p := os.Getenv("MERA_LOG_FILE"); p != "" {
		f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644)
		if err != nil {
			log.Printf("warning: could not open log file %s, logging to stdout only: %v", p, err)
		} else {
			log.SetOutput(io.MultiWriter(os.Stdout, f))
		}
	}

	srv := api.NewServer(workRoot, mxRoot)

	addr := ":" + port
	log.Printf("extractor listening on %s (workRoot=%s)", addr, workRoot)
	log.Fatal(http.ListenAndServe(addr, srv.Routes()))
}
