package browser

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pkgbrowser "github.com/pkg/browser"
)

// TestOpen pins the wait that stands between a ready tunnel and a browser,
// and the launch that follows it. The edge answers 530 for a moment after
// TunnelReady fires, and a tab opened into that window shows an error page
// for a tunnel that works; the launch itself never writes to stdout and never
// raises its voice above a debug line when it fails.
func TestOpen(t *testing.T) {
	// returns once the edge stops failing: the launcher fires only after the
	// probe has kept trying until the edge finally answers.
	t.Run("keeps probing until the edge answers", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) <= 3 {
				w.WriteHeader(http.StatusBadGateway) // the edge, not the origin
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		var seen int32
		o := New(
			WithLaunch(func(string) error {
				seen = calls.Load()
				return nil
			}),
			WithWindow(5*time.Second),
		)
		o.Open(t.Context(), srv.URL, io.Discard, slog.New(slog.DiscardHandler))

		if seen < 4 {
			t.Errorf("gave up after %d probes, want it to keep trying until the edge answered", seen)
		}
	})

	// an origin's own error still counts as reachable: the route is live,
	// the app simply has nothing at this path, so the probe stops there.
	t.Run("an origin's own error counts as reachable", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			// The route is live; the app simply has nothing at this path.
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		var seen int32
		o := New(
			WithLaunch(func(string) error {
				seen = calls.Load()
				return nil
			}),
			WithWindow(5*time.Second),
		)
		o.Open(t.Context(), srv.URL, io.Discard, slog.New(slog.DiscardHandler))

		if seen != 1 {
			t.Errorf("probed %d times, want it to stop at the first answer from the origin", seen)
		}
	})

	// gives up rather than never opening: an edge that only ever answers 503
	// still gets a browser once the window elapses, and the debug log says so.
	t.Run("gives up rather than never opening", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		var logged bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

		var launched bool
		o := New(
			WithLaunch(func(string) error {
				launched = true
				return nil
			}),
			WithWindow(600*time.Millisecond),
		)

		start := time.Now()
		o.Open(t.Context(), srv.URL, io.Discard, log)
		elapsed := time.Since(start)

		if elapsed > 3*time.Second {
			t.Errorf("waited %v, want it bounded by the window it was given", elapsed)
		}
		if !launched {
			t.Error("gave up on the wait but never launched the browser")
		}
		if !strings.Contains(logged.String(), "before the edge answered") {
			t.Errorf("log = %q, want the debug log to say it opened anyway", logged.String())
		}
	})

	// a cancelled context ends the wait: Open must return promptly rather
	// than block forever on a context that will never resolve on its own.
	t.Run("a cancelled context ends the wait", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		o := New(WithLaunch(func(string) error { return nil }), WithWindow(time.Minute))

		done := make(chan struct{})
		go func() {
			defer close(done)
			o.Open(ctx, srv.URL, io.Discard, slog.New(slog.DiscardHandler))
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("Open ignored a cancelled context; Ctrl-C would hang")
		}
	})

	// opens the address and leaves stdout alone: the spawned process
	// inherits writers from pkg/browser's package globals, which default to
	// os.Stdout, and that stream carries nothing but what a script reads.
	t.Run("opens the address and leaves stdout alone", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		var opened string
		o := New(
			WithLaunch(func(addr string) error {
				opened = addr
				return nil
			}),
			WithWindow(5*time.Second),
		)

		var stderr bytes.Buffer
		o.Open(t.Context(), srv.URL, &stderr, slog.New(slog.DiscardHandler))

		if opened != srv.URL {
			t.Errorf("opened %q, want %q", opened, srv.URL)
		}
		if pkgbrowser.Stdout != io.Writer(&stderr) {
			t.Error("browser.Stdout was left pointing elsewhere, want the stderr writer")
		}
		if pkgbrowser.Stderr != io.Writer(&stderr) {
			t.Error("browser.Stderr was left pointing elsewhere, want the stderr writer")
		}
	})

	// a failure to open is a debug line, not an error: the tunnel is up and
	// serving either way, and a headless host is a normal place to run this,
	// not a broken one. A headless host — a server, a container, CI — must
	// never hear about it above that level, and it must still be there for
	// somebody debugging a browser that did not appear.
	t.Run("a failure is quiet outside the debug log", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		launch := func(string) error { return errors.New("no browser here") }

		var quiet bytes.Buffer
		o := New(WithLaunch(launch), WithWindow(5*time.Second))
		o.Open(t.Context(), srv.URL, io.Discard,
			slog.New(slog.NewTextHandler(&quiet, &slog.HandlerOptions{Level: slog.LevelWarn})))
		if quiet.Len() != 0 {
			t.Errorf("log = %q, want nothing at warn level", quiet.String())
		}

		var logged bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
		o.Open(t.Context(), srv.URL, io.Discard, log)

		if !strings.Contains(logged.String(), "could not open a browser") {
			t.Errorf("log = %q, want the failure in the debug log", logged.String())
		}
	})

	// pins the order the two halves run in: the probe first, then the
	// launch, so a browser is never pointed at an address the edge has not
	// answered for. The server counts probes; the launcher must see the
	// count already settled.
	t.Run("probes before it launches", func(t *testing.T) {
		var probes atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if probes.Add(1) <= 2 {
				w.WriteHeader(http.StatusBadGateway)
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		var launched string
		var seen int32
		o := New(
			WithLaunch(func(addr string) error {
				launched, seen = addr, probes.Load()
				return nil
			}),
			WithWindow(5*time.Second),
		)
		o.Open(t.Context(), srv.URL, io.Discard, slog.New(slog.DiscardHandler))

		if launched != srv.URL {
			t.Errorf("launched %q, want %q", launched, srv.URL)
		}
		if seen < 3 {
			t.Errorf("launched after %d probes, want the probe to have succeeded first", seen)
		}
	})
}
