package router

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"
)

// requests is a line per request to an origin the tunnel dials, on a run's
// own terminal when no frame is drawn on it: the request-log half of compose
// output, beside each program's lines.
type requests struct {
	mu       sync.Mutex
	w        io.Writer
	prefixes []string
}

// WithRequestLog writes a line per request an origin answers to w, behind
// prefixes[index] for the origin it reached ("" for one not logged): method,
// path, status and how long it took, e.g. "[:3000] GET / 200 12ms".
func WithRequestLog(w io.Writer, prefixes []string) Option {
	return func(r *RouterImpl) { r.requests = &requests{w: w, prefixes: prefixes} }
}

// pickedKey carries a *int proxy fills with the index of the origin it chose.
type pickedKey struct{}

// logged writes reqs' line for every request next sends to an origin.
func logged(reqs *requests, next http.Handler) http.Handler {
	if reqs == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		ix := -1
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r.WithContext(context.WithValue(r.Context(), pickedKey{}, &ix)))
		if ix < 0 || ix >= len(reqs.prefixes) || reqs.prefixes[ix] == "" {
			return
		}
		code := sw.code
		if code == 0 {
			code = http.StatusOK
		}
		line := fmt.Sprintf("%s%s %s %d %s\n", reqs.prefixes[ix], r.Method, r.URL.RequestURI(), code, time.Since(start).Round(time.Millisecond))
		reqs.mu.Lock()
		_, _ = io.WriteString(reqs.w, line)
		reqs.mu.Unlock()
	})
}

// statusWriter keeps the status an answer was given.
type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

// Unwrap is for http.ResponseController: the proxy flushes and upgrades
// through it.
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }
