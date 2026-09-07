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

	pkgbrowser "github.com/pkg/browser"
)

// TestOpen pins the launch and the two promises around it: it never writes to
// stdout, and it never raises its voice above a debug line when it fails.
//
// The wait that used to stand in front of it lives in the counter now — the
// edge answers 530 for a moment after a tunnel reports ready, and its own
// event stream says when that is over, which an HTTP probe from here could
// only guess at.
func TestOpen(t *testing.T) {
	// opens the address and leaves stdout alone: the spawned process
	// inherits writers from pkg/browser's package globals, which default to
	// os.Stdout, and that stream carries a running tunnel's addresses.
	t.Run("opens the address and leaves stdout alone", func(t *testing.T) {
		var opened string
		o := New(WithLaunch(func(addr string) error {
			opened = addr
			return nil
		}))

		var stderr bytes.Buffer
		o.Open(t.Context(), "https://striped-worm.tunneled.pizza/", &stderr, slog.New(slog.DiscardHandler))

		if want := "https://striped-worm.tunneled.pizza/"; opened != want {
			t.Errorf("opened %q, want %q", opened, want)
		}
		if pkgbrowser.Stdout != io.Writer(&stderr) {
			t.Error("browser.Stdout was left pointing elsewhere, want the stderr writer")
		}
		if pkgbrowser.Stderr != io.Writer(&stderr) {
			t.Error("browser.Stderr was left pointing elsewhere, want the stderr writer")
		}
	})

	// asks the address nothing: the probe moved to the counter, and an Open
	// that still reached for the network would wait twice for one answer.
	t.Run("reaches for the network not at all", func(t *testing.T) {
		var requests atomic.Int32
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			requests.Add(1)
			w.WriteHeader(http.StatusBadGateway) // the edge, not the origin
		}))
		defer srv.Close()

		o := New(WithLaunch(func(string) error { return nil }))
		o.Open(t.Context(), srv.URL, io.Discard, slog.New(slog.DiscardHandler))

		if got := requests.Load(); got != 0 {
			t.Errorf("made %d requests to the address, want none", got)
		}
	})

	// a dead context still launches: Open does not wait, so there is nothing
	// for a cancellation to cut short, and refusing to launch would only
	// withhold a page from somebody whose tunnel is up.
	t.Run("a cancelled context still launches", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		var launched bool
		o := New(WithLaunch(func(string) error {
			launched = true
			return nil
		}))
		o.Open(ctx, "https://striped-worm.tunneled.pizza/", io.Discard, slog.New(slog.DiscardHandler))

		if !launched {
			t.Error("a cancelled context stopped the launch, want it to open anyway")
		}
	})

	// a failure to open is a debug line, not an error: the tunnel is up and
	// serving either way, and a headless host is a normal place to run this,
	// not a broken one. It must still be there for somebody debugging a
	// browser that did not appear.
	t.Run("a failure is quiet outside the debug log", func(t *testing.T) {
		o := New(WithLaunch(func(string) error { return errors.New("no browser here") }))

		var quiet bytes.Buffer
		o.Open(t.Context(), "https://striped-worm.tunneled.pizza/", io.Discard,
			slog.New(slog.NewTextHandler(&quiet, &slog.HandlerOptions{Level: slog.LevelWarn})))
		if quiet.Len() != 0 {
			t.Errorf("log = %q, want nothing at warn level", quiet.String())
		}

		var logged bytes.Buffer
		o.Open(t.Context(), "https://striped-worm.tunneled.pizza/", io.Discard,
			slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug})))
		if !strings.Contains(logged.String(), "could not open a browser") {
			t.Errorf("log = %q, want the failure in the debug log", logged.String())
		}
	})
}
