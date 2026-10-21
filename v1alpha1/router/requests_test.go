package router

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"regexp"
	"testing"
)

// TestLoggedWritesALinePerRequest pins the request log: a line per request,
// behind the prefix of the origin it reached, with its method, path, status
// and duration; nothing for an origin with no prefix (a served one).
func TestLoggedWritesALinePerRequest(t *testing.T) {
	var out bytes.Buffer
	reqs := &requests{w: &out, prefixes: []string{"[:3000] ", ""}}
	to := 0
	next := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*r.Context().Value(pickedKey{}).(*int) = to
		w.WriteHeader(http.StatusNotFound)
	})
	h := logged(reqs, next)

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/missing?x=1", nil))
	if !regexp.MustCompile(`^\[:3000\] GET /missing\?x=1 404 \d+m?s\n$`).MatchString(out.String()) {
		t.Errorf("logged %q, want one line for the request", out.String())
	}

	out.Reset()
	to = 1
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "/", nil))
	if out.Len() != 0 {
		t.Errorf("logged %q for an origin with no prefix, want nothing", out.String())
	}
}
