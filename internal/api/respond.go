// internal/api/respond.go
package api

import (
	"encoding/json"
	"log"
	"net/http"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

// respondError is the single choke point every error response passes
// through, which is what makes the two rules below hold everywhere rather
// than depending on each call site remembering them:
//
//  1. The real error is always logged server-side, unconditionally. Nothing
//     else in this package logs — see MERA-session-status.md for the
//     request that surfaced this: a 500 that produced no server-side trace
//     at all, because nothing in the request path called log.* anywhere.
//  2. For 5xx, the caller gets a generic message instead of err.Error().
//     5xx means our code, our subprocess, or our filesystem — those
//     messages can carry server-side absolute paths (see describeMxRoot)
//     or raw subprocess stderr, neither of which is for the client. 4xx and
//     502 stay verbatim: those are the caller's own input, or an upstream
//     failure already sanitized where it could carry a credential (see
//     gitops.redact) — that detail is meant to be actionable, not hidden.
func respondError(w http.ResponseWriter, r *http.Request, status int, err error) {
	log.Printf("%s %s: %d: %v", r.Method, r.URL.Path, status, err)

	msg := err.Error()
	if status >= http.StatusInternalServerError {
		msg = "internal error"
	}
	writeJSON(w, status, map[string]string{"error": msg})
}