// internal/api/accesslog.go
package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Access logging: one "→" line when a request arrives, one "←" line when it
// finishes, both carrying the same rid so a slow /extract's two lines can be
// paired up in the log even with other requests interleaved between them.
//
// The middleware only knows transport-level facts (who connected, which
// route, status, duration). Handlers add the domain-level facts — requestId,
// repo, SHAs, result counts — via annotate, and those ride on the "←" line.
// That keeps one summary line per request instead of a trail of partial ones.
//
// ▶ Why log metadata only, never bodies: request bodies carry a PAT, and
// response bodies are MDL dumps that can run to megabytes. Neither belongs in
// a size-capped rotating log. Every field below is chosen to be safe to keep.

type ctxKey int

const reqLogKey ctxKey = 0

// reqLog is the per-request state the middleware hands down via the context.
type reqLog struct {
	rid string

	mu      sync.Mutex
	fields  []string // "k=v" pairs, in the order handlers added them
	routine bool     // see markRoutine
}

// requestIDHeader is honoured on the way in and always set on the way out.
// A caller that sends its own ID gets it back and sees it in our logs; one
// that doesn't gets ours, so either side can quote it when chasing a problem.
const requestIDHeader = "X-Request-Id"

// withAccessLog wraps next with the "→" / "←" log lines.
func withAccessLog(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()

		rid := sanitizeRID(r.Header.Get(requestIDHeader))
		if rid == "" {
			rid = newRID()
		}
		rl := &reqLog{rid: rid}
		r = r.WithContext(context.WithValue(r.Context(), reqLogKey, rl))
		w.Header().Set(requestIDHeader, rid)

		// A probe's "→" line is held back until we know whether it's worth
		// printing at all; see isProbe. Every other request logs on arrival,
		// so a long /extract is visible while it's still running.
		probe := isProbe(r)
		arrival := fmt.Sprintf("http rid=%s → %s %s %s", rid, r.Method, r.URL.Path, callerFields(r))
		if !probe {
			log.Print(arrival)
		}

		rec := &statusRecorder{ResponseWriter: w}
		defer func() {
			// A panic is logged here and then re-raised, so net/http's own
			// recovery still runs. Without this the "←" line would simply be
			// missing and the "→" line would look like a request still in flight.
			p := recover()
			status := rec.status
			if p != nil {
				status = http.StatusInternalServerError
			} else if status == 0 {
				status = http.StatusOK // handler wrote nothing; net/http sends 200
			}
			line := fmt.Sprintf("http rid=%s ← %s %s status=%d dur=%s bytes=%d",
				rid, r.Method, r.URL.Path, status, time.Since(start).Round(time.Millisecond), rec.bytes)
			if f := rl.joined(); f != "" {
				line += " " + f
			}
			if p != nil {
				line += fmt.Sprintf(" panic=%q", fmt.Sprint(p))
			}
			if probe {
				if p == nil && status == http.StatusOK && rl.isRoutine() {
					return
				}
				log.Print(arrival)
			}
			if ctxErr := r.Context().Err(); ctxErr != nil {
				// The caller hung up (or timed out) before we finished — worth
				// knowing when a long /extract "never returned" on their side.
				line += fmt.Sprintf(" ctx=%q", ctxErr.Error())
			}
			log.Print(line)
			if p != nil {
				panic(p)
			}
		}()
		next.ServeHTTP(rec, r)
	})
}

// isProbe reports whether r looks like the container's own healthcheck:
// GET /health from loopback. Docker runs it every 30s from inside the
// container (compose's healthcheck), which is ~5,800 log line pairs a day
// saying nothing, in a size-capped log. Docker keeps those results itself:
// `docker inspect mera-extractor --format '{{json .State.Health}}'`.
//
// ▶ Why loopback and not the user agent: a UA is caller-controlled, so
// matching "curl" would hide any curl from anywhere. Loopback can't be
// spoofed from outside the container. A curl from the host via the
// published port arrives from the docker gateway (e.g. 172.18.0.1), not
// loopback, so it is still logged. The exception is `go run` outside Docker,
// where a local curl to /health is loopback too and an ok answer goes quiet.
func isProbe(r *http.Request) bool {
	if r.Method != http.MethodGet || r.URL.Path != "/health" {
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// markRoutine tells the access log that this request's outcome is
// uneventful. Only probes (isProbe) act on it: such a request is then not
// logged at all. A handler calls it only for its healthy path, so a degraded
// /health, a non-200 or a panic is still logged in full, both lines.
func markRoutine(r *http.Request) {
	if rl, _ := r.Context().Value(reqLogKey).(*reqLog); rl != nil {
		rl.mu.Lock()
		rl.routine = true
		rl.mu.Unlock()
	}
}

func (rl *reqLog) isRoutine() bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return rl.routine
}

// callerFields is "who is calling". RemoteAddr is the TCP peer and can't be
// forged; X-Forwarded-For can, so it is logged as its own field, never in
// place of RemoteAddr — behind a proxy you want both, and only the operator
// knows which one to trust.
func callerFields(r *http.Request) string {
	parts := []string{"remote=" + r.RemoteAddr}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		parts = append(parts, fmt.Sprintf("xff=%q", truncate(xff, 200)))
	}
	if ua := r.UserAgent(); ua != "" {
		parts = append(parts, fmt.Sprintf("ua=%q", truncate(ua, 200)))
	}
	if r.ContentLength >= 0 {
		parts = append(parts, fmt.Sprintf("reqBytes=%d", r.ContentLength))
	}
	return strings.Join(parts, " ")
}

// annotate adds key=value metadata to this request's "←" line. Values are
// %q-quoted when they contain anything that would break a naive key=value
// split. A no-op when the request didn't come through withAccessLog — which
// is every handler test, since those call the handlers directly.
func annotate(r *http.Request, kv ...any) {
	rl, _ := r.Context().Value(reqLogKey).(*reqLog)
	if rl == nil {
		return
	}
	rl.mu.Lock()
	defer rl.mu.Unlock()
	for i := 0; i+1 < len(kv); i += 2 {
		rl.fields = append(rl.fields, fmt.Sprintf("%v=%s", kv[i], logValue(kv[i+1])))
	}
}

// requestRID returns the rid for r, or "-" outside the middleware.
func requestRID(r *http.Request) string {
	if rl, _ := r.Context().Value(reqLogKey).(*reqLog); rl != nil {
		return rl.rid
	}
	return "-"
}

func (rl *reqLog) joined() string {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	return strings.Join(rl.fields, " ")
}

func logValue(v any) string {
	s := fmt.Sprint(v)
	if s == "" || strings.ContainsAny(s, " \t\n\"=") {
		return fmt.Sprintf("%q", truncate(s, 300))
	}
	return truncate(s, 300)
}

// redactURL strips userinfo from a repo URL before it is logged. The API
// takes credentials as separate fields, but nothing stops a caller pasting
// https://user:pat@host/... into repoUrl, and that must not land in a log.
// An unparseable URL is replaced outright rather than logged raw, for the
// same reason.
func redactURL(raw string) string {
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "<unparseable>"
	}
	u.User = nil
	return u.String()
}

// shortSha keeps logs scannable; 12 hex chars is unambiguous in any repo
// this service will ever see.
func shortSha(s string) string {
	return truncate(s, 12)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

// sanitizeRID accepts a caller-supplied ID only if it's short and plain, so a
// hostile header can't inject spaces or newlines into our log lines.
func sanitizeRID(s string) string {
	if s == "" || len(s) > 64 {
		return ""
	}
	for _, c := range s {
		ok := c == '-' || c == '_' || c == '.' ||
			(c >= '0' && c <= '9') || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		if !ok {
			return ""
		}
	}
	return s
}

func newRID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("t%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b[:])
}

// statusRecorder captures what the handler sent, for the "←" line.
type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += int64(n)
	return n, err
}

// Unwrap lets http.ResponseController reach the real writer (Flush, deadlines).
func (s *statusRecorder) Unwrap() http.ResponseWriter { return s.ResponseWriter }
