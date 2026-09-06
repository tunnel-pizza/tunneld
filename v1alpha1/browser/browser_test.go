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

// TestOpen pins the order the two halves run in: the probe first, then the
// launch, so a browser is never pointed at an address the edge has not
// answered for. The server counts probes; the launcher must see the count
// already settled.
func TestOpen(t *testing.T) {
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
}

// TestOpenInBrowser pins the two things the launcher promises. It never writes
// to stdout — the spawned process inherits writers from pkg/browser's package
// globals, which default to os.Stdout, and that stream carries nothing but
// what a script reads. And a failure to open is a debug line, not an error:
// the tunnel is up and serving either way, and a headless host is a normal
// place to run this, not a broken one.
func TestOpenInBrowser(t *testing.T) {
	t.Run("opens the address and leaves stdout alone", func(t *testing.T) {
		var opened string
		launch := func(addr string) error {
			opened = addr
			return nil
		}

		var stderr bytes.Buffer
		openInBrowser(launch, "https://foo.tunneled.pizza/", &stderr, slog.New(slog.DiscardHandler))

		if want := "https://foo.tunneled.pizza/"; opened != want {
			t.Errorf("opened %q, want %q", opened, want)
		}
		if pkgbrowser.Stdout != io.Writer(&stderr) {
			t.Error("browser.Stdout was left pointing elsewhere, want the stderr writer")
		}
		if pkgbrowser.Stderr != io.Writer(&stderr) {
			t.Error("browser.Stderr was left pointing elsewhere, want the stderr writer")
		}
	})

	// A headless host — a server, a container, CI — is a normal place to run
	// this, not a broken one, so the failure must not reach an operator who
	// never asked about it. Both halves matter: silent at warn, and still
	// there for somebody debugging a browser that did not appear.
	t.Run("a failure is quiet outside the debug log", func(t *testing.T) {
		launch := func(string) error { return errors.New("no browser here") }

		var quiet bytes.Buffer
		openInBrowser(launch, "https://foo.tunneled.pizza/", io.Discard,
			slog.New(slog.NewTextHandler(&quiet, &slog.HandlerOptions{Level: slog.LevelWarn})))
		if quiet.Len() != 0 {
			t.Errorf("log = %q, want nothing at warn level", quiet.String())
		}

		var logged bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
		openInBrowser(launch, "https://foo.tunneled.pizza/", io.Discard, log)

		if !strings.Contains(logged.String(), "could not open a browser") {
			t.Errorf("log = %q, want the failure in the debug log", logged.String())
		}
	})
}

// TestAwaitReachable pins the wait that stands between a ready tunnel and a
// browser. The edge answers 530 for a moment after TunnelReady fires, and a
// tab opened into that window shows an error page for a tunnel that works.
func TestAwaitReachable(t *testing.T) {
	t.Run("returns once the edge stops failing", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			if calls.Add(1) <= 3 {
				w.WriteHeader(http.StatusBadGateway) // the edge, not the origin
				return
			}
			w.WriteHeader(http.StatusOK)
		}))
		defer srv.Close()

		awaitReachable(t.Context(), srv.URL, 5*time.Second, slog.New(slog.DiscardHandler))

		if got := calls.Load(); got < 4 {
			t.Errorf("gave up after %d probes, want it to keep trying until the edge answered", got)
		}
	})

	t.Run("an origin's own error still counts as reachable", func(t *testing.T) {
		var calls atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			calls.Add(1)
			// The route is live; the app simply has nothing at this path.
			w.WriteHeader(http.StatusNotFound)
		}))
		defer srv.Close()

		awaitReachable(t.Context(), srv.URL, 5*time.Second, slog.New(slog.DiscardHandler))

		if got := calls.Load(); got != 1 {
			t.Errorf("probed %d times, want it to stop at the first answer from the origin", got)
		}
	})

	t.Run("gives up rather than never opening", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer srv.Close()

		var logged bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))

		start := time.Now()
		awaitReachable(t.Context(), srv.URL, 600*time.Millisecond, log)
		elapsed := time.Since(start)

		if elapsed > 3*time.Second {
			t.Errorf("waited %v, want it bounded by the timeout it was given", elapsed)
		}
		if !strings.Contains(logged.String(), "before the edge answered") {
			t.Errorf("log = %q, want the debug log to say it opened anyway", logged.String())
		}
	})

	t.Run("a cancelled context ends the wait", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusBadGateway)
		}))
		defer srv.Close()

		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		done := make(chan struct{})
		go func() {
			defer close(done)
			awaitReachable(ctx, srv.URL, time.Minute, slog.New(slog.DiscardHandler))
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("awaitReachable ignored a cancelled context; Ctrl-C would hang")
		}
	})
}
