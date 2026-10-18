// internal/api/accesslog_test.go
package api

import (
	"bytes"
	"context"
	"errors"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"mera-extractor/internal/gitops"
	"mera-extractor/internal/mx"
)

// captureLog redirects the standard logger for the duration of the test.
func captureLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prevOut, prevFlags := log.Writer(), log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	t.Cleanup(func() { log.SetOutput(prevOut); log.SetFlags(prevFlags) })
	return &buf
}

func TestAccessLog_StartAndEndLinesShareRID(t *testing.T) {
	buf := captureLog(t)
	h := newHarness(t)

	r := httptest.NewRequest(http.MethodPost, "/extract",
		strings.NewReader(`{"requestId":"req-1","repoUrl":"https://git.example/x.git","baseSha":"aaaaaaa","headSha":"bbbbbbb"}`))
	r.Header.Set("User-Agent", "mera-test/1")
	w := httptest.NewRecorder()
	h.srv.Routes().ServeHTTP(w, r)

	rid := w.Header().Get(requestIDHeader)
	if rid == "" {
		t.Fatal("no X-Request-Id on the response")
	}
	out := buf.String()
	for _, want := range []string{
		"http rid=" + rid + " → POST /extract",
		`ua="mera-test/1"`,
		"http rid=" + rid + " extract started requestId=req-1",
		"http rid=" + rid + " ← POST /extract status=200",
		"requestId=req-1", "base=aaaaaaa", "head=bbbbbbb", "path=diff", "changeUnits=",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("log missing %q\n%s", want, out)
		}
	}
}

func TestAccessLog_NeverLogsCredentials(t *testing.T) {
	buf := captureLog(t)
	h := newHarness(t)
	h.srv.Deps.CloneBoth = func(_ context.Context, _ string, _ gitops.CloneBothRequest) (gitops.CloneBothResult, error) {
		return gitops.CloneBothResult{}, errors.New("auth failed")
	}

	r := httptest.NewRequest(http.MethodPost, "/extract", strings.NewReader(
		`{"repoUrl":"https://bob:urlsecret@git.example/x.git","username":"bob","pat":"patsecret","baseSha":"a","headSha":"b"}`))
	w := httptest.NewRecorder()
	h.srv.Routes().ServeHTTP(w, r)

	out := buf.String()
	if strings.Contains(out, "patsecret") || strings.Contains(out, "urlsecret") {
		t.Fatalf("credential leaked into log:\n%s", out)
	}
	if !strings.Contains(out, "user=bob") || !strings.Contains(out, "status=502") {
		t.Errorf("expected caller identity and status in log:\n%s", out)
	}
}

func TestAccessLog_HonoursSaneCallerRID(t *testing.T) {
	captureLog(t)
	srv := healthServer(t.TempDir(), nil, nil, "v0", nil)

	cases := map[string]bool{
		"caller-123":            true,
		"bad id\nwith newline":  false,
		strings.Repeat("x", 65): false,
	}
	for in, keep := range cases {
		r := httptest.NewRequest(http.MethodGet, "/health", nil)
		r.Header.Set(requestIDHeader, in)
		w := httptest.NewRecorder()
		srv.Routes().ServeHTTP(w, r)
		got := w.Header().Get(requestIDHeader)
		if keep != (got == in) {
			t.Errorf("rid %q: got %q, keep=%v", in, got, keep)
		}
	}
}

func TestRedactURL(t *testing.T) {
	for in, want := range map[string]string{
		"https://git.example/x.git":     "https://git.example/x.git",
		"https://u:p@git.example/x.git": "https://git.example/x.git",
		"":                              "",
		"://bad":                        "<unparseable>",
	} {
		if got := redactURL(in); got != want {
			t.Errorf("redactURL(%q) = %q, want %q", in, got, want)
		}
	}
}

// The container's own healthcheck (GET /health from loopback) is silent while
// healthy, and fully logged the moment it isn't — or when anyone else calls.
func TestAccessLog_HealthyProbeIsQuiet(t *testing.T) {
	cases := []struct {
		name     string
		remote   string
		mxcliErr error
		logged   bool
	}{
		{"loopback v4, ok", "127.0.0.1:5000", nil, false},
		{"loopback v6, ok", "[::1]:5000", nil, false},
		{"loopback, degraded", "127.0.0.1:5000", errors.New("no mxcli"), true},
		{"docker gateway, ok", "172.18.0.1:5000", nil, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			buf := captureLog(t)
			srv := healthServer(t.TempDir(), []mx.Binary{{Version: "11.12.6"}}, nil, "v0", tc.mxcliErr)
			r := httptest.NewRequest(http.MethodGet, "/health", nil)
			r.RemoteAddr = tc.remote
			srv.Routes().ServeHTTP(httptest.NewRecorder(), r)

			out := buf.String()
			hasBoth := strings.Contains(out, "→ GET /health") && strings.Contains(out, "← GET /health")
			if tc.logged && !hasBoth {
				t.Errorf("expected both access-log lines, got:\n%s", out)
			}
			if !tc.logged && out != "" {
				t.Errorf("expected no log output, got:\n%s", out)
			}
		})
	}
}
