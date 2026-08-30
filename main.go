// main.go
package main

import (
	"context"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/joho/godotenv"

	"mera-extractor/internal/api"
	"mera-extractor/internal/logrotate"
	"mera-extractor/internal/mx"
	"mera-extractor/internal/mxcli"
	"mera-extractor/internal/workspace"
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
		// Rotate on the calendar-day boundary and at a size cap, so
		// /logs/extractor.log never grows without bound. Files are named
		// extractor-YYYY-MM-DD.log, with a .N iterator when a day needs more
		// than one file (see internal/logrotate).
		maxBytes := int64(10 << 20)
		if v := os.Getenv("MERA_LOG_MAX_BYTES"); v != "" {
			if n, err := strconv.ParseInt(v, 10, 64); err == nil && n > 0 {
				maxBytes = n
			} else {
				log.Printf("MERA_LOG_MAX_BYTES=%q invalid, keeping default %d", v, maxBytes)
			}
		}
		w, err := logrotate.New(p, maxBytes)
		if err != nil {
			log.Printf("warning: could not open log file %s, logging to stdout only: %v", p, err)
		} else {
			// stdout first: if the log disk fills or turns read-only, the
			// container still gets console output before MultiWriter aborts.
			log.SetOutput(io.MultiWriter(os.Stdout, w))
		}
	}

	var wsOpts []workspace.Option
	if v := os.Getenv("MERA_WORKSPACE_TTL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			wsOpts = append(wsOpts, workspace.WithTTL(d))
		} else {
			log.Printf("MERA_WORKSPACE_TTL=%q invalid, keeping default %s", v, workspace.DefaultTTL)
		}
	}
	wsm, err := workspace.New(workRoot, wsOpts...)
	if err != nil {
		// Without a writable workspace dir the service cannot clone anything.
		log.Fatalf("workspace init under %s: %v", workRoot, err)
	}
	// context.Background() is deliberate: the janitor's lifetime is the
	// process's. main ends with log.Fatal(ListenAndServe), so there is no
	// graceful-shutdown path to thread a cancel through, and a reap killed
	// mid-tick leaves at worst a half-removed clone-* dir that the next tick
	// (or the next startup sweep) finishes.
	wsm.StartJanitor(context.Background())
	log.Printf("workspace ready: %s", wsm.Describe())

	srv := api.NewServer(wsm.Dir(), mxRoot)
	srv.Workspace = wsm

	addr := ":" + port
	log.Printf("extractor listening on %s (workRoot=%s)", addr, wsm.Dir())
	log.Fatal(http.ListenAndServe(addr, srv.Routes()))
}
