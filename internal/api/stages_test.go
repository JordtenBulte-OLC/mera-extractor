// internal/api/stages_test.go
package api

import (
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

func TestStageTimer_OrderAndUnfinished(t *testing.T) {
	var st stageTimer
	if got := st.String(); got != "" {
		t.Errorf("fresh timer = %q, want empty", got)
	}
	st.start("clone")
	st.start("diff") // ends clone
	got := st.String()
	if !regexp.MustCompile(`^clone:[0-9.]+m?s,diff:[0-9.]+m?s\(unfinished\)$`).MatchString(got) {
		t.Errorf("running timer = %q, want clone finished and diff (unfinished)", got)
	}
	st.stop()
	st.stop() // idempotent
	got = st.String()
	if strings.Contains(got, "unfinished") || strings.Count(got, ",") != 1 {
		t.Errorf("stopped timer = %q, want exactly clone and diff, both finished", got)
	}
}

// The "←" line is the whole point: it must carry every diff-path stage, in
// the order they ran.
func TestAccessLog_ExtractCarriesStages(t *testing.T) {
	buf := captureLog(t)
	h := newHarness(t)

	r := httptest.NewRequest(http.MethodPost, "/extract",
		strings.NewReader(`{"requestId":"req-1","repoUrl":"https://git.example/x.git","baseSha":"aaaaaaa","headSha":"bbbbbbb"}`))
	h.srv.Routes().ServeHTTP(httptest.NewRecorder(), r)

	m := regexp.MustCompile(`← POST /extract status=200 .* stages=(\S+)`).FindStringSubmatch(buf.String())
	if m == nil {
		t.Fatalf("no stages= on the ← line:\n%s", buf.String())
	}
	var names []string
	for _, part := range strings.Split(m[1], ",") {
		names = append(names, strings.SplitN(part, ":", 2)[0])
	}
	want := "clone,prepare,diff,resolve,describe,textdiffs,version"
	if got := strings.Join(names, ","); got != want {
		t.Errorf("stages = %s, want %s", got, want)
	}
}

// A failed request still reports how far it got.
func TestAccessLog_FailedExtractCarriesStagesSoFar(t *testing.T) {
	buf := captureLog(t)
	h := newHarness(t)
	h.srv.Deps.MxHighest = nil // falls back to the real lookup under /opt/mx...
	h.srv.MxRoot = t.TempDir() // ...which is empty, so prepare fails

	r := httptest.NewRequest(http.MethodPost, "/extract",
		strings.NewReader(`{"repoUrl":"https://git.example/x.git","baseSha":"aaaaaaa","headSha":"bbbbbbb"}`))
	h.srv.Routes().ServeHTTP(httptest.NewRecorder(), r)

	if !regexp.MustCompile(`← POST /extract status=500 .*stages=clone:[^,]+,prepare:`).MatchString(buf.String()) {
		t.Errorf("want clone and prepare stages on the failed ← line:\n%s", buf.String())
	}
}
