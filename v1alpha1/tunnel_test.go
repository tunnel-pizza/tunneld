package v1alpha1

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/pkg/browser"
	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/multiview"
)

// TestParseOriginsAccepts covers the shapes a caller is allowed to type,
// including the bare host:port that implies http — the affordance that lets
// `--url localhost:3000` work the way people expect.
func TestParseOriginsAccepts(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"explicit http", []string{"http://localhost:3000"}, []string{"http://localhost:3000"}},
		{"https origin", []string{"https://127.0.0.1:8443"}, []string{"https://127.0.0.1:8443"}},
		{"bare host:port implies http", []string{"localhost:3000"}, []string{"http://localhost:3000"}},
		{"bare host implies http", []string{"localhost"}, []string{"http://localhost"}},
		{"bare port implies localhost", []string{":8000"}, []string{"http://localhost:8000"}},
		{"bare port keeps an explicit scheme", []string{"https://:8443"}, []string{"https://localhost:8443"}},
		{"scheme and bare port", []string{"http://:8000"}, []string{"http://localhost:8000"}},
		{"bare port with a path", []string{":8000/api"}, []string{"http://localhost:8000/api"}},
		{"surrounding space trimmed", []string{"  http://localhost:3000  "}, []string{"http://localhost:3000"}},
		{"path preserved", []string{"http://localhost:3000/api"}, []string{"http://localhost:3000/api"}},
		{
			"order preserved across origins",
			[]string{"http://localhost:3000", "http://localhost:4000"},
			[]string{"http://localhost:3000", "http://localhost:4000"},
		},
		{"a container by name", []string{"dockerd://api"}, []string{"dockerd://api"}},
		{"a container by id", []string{"dockerd://3f2a1b9c8d7e"}, []string{"dockerd://3f2a1b9c8d7e"}},
		{"a container name keeps case and underscores", []string{"dockerd://My_Container"}, []string{"dockerd://My_Container"}},
		{"a websocket-owning origin keeps its marker", []string{"http://localhost:4000", "http+ws://localhost:5173"}, []string{"http://localhost:4000", "http+ws://localhost:5173"}},
		{"the marker on https", []string{"http://localhost:4000", "https+wss://localhost:5173"}, []string{"http://localhost:4000", "https+wss://localhost:5173"}},
		{"ws and wss are interchangeable", []string{"http://localhost:4000", "http+wss://localhost:5173"}, []string{"http://localhost:4000", "http+wss://localhost:5173"}},
		{"a marked origin keeps the bare-port shorthand", []string{"http://localhost:4000", "http+ws://:5173"}, []string{"http://localhost:4000", "http+ws://localhost:5173"}},
		{
			"a container beside an http origin, in order",
			[]string{"http://localhost:3000", "dockerd://api"},
			[]string{"http://localhost:3000", "dockerd://api"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOrigins(tc.in)
			if err != nil {
				t.Fatalf("parseOrigins(%q) = error %v, want ok", tc.in, err)
			}
			if len(got) != len(tc.want) {
				t.Fatalf("parseOrigins(%q) returned %d origins, want %d", tc.in, len(got), len(tc.want))
			}
			for i, u := range got {
				if u.String() != tc.want[i] {
					t.Errorf("origin %d = %q, want %q", i, u, tc.want[i])
				}
			}
		})
	}
}

// TestParseOriginsRejects pins the failure modes as errors rather than as a
// public hostname that answers only errors. Each case asserts both the sentinel
// (so callers can branch on the class) and that the message names the offending
// input (so an operator can act on it).
func TestParseOriginsRejects(t *testing.T) {
	cases := []struct {
		name    string
		in      []string
		want    error
		mention string
	}{
		{"no origins at all", nil, v1.ErrNoOrigin, ""},
		{"empty value", []string{""}, v1.ErrNoOrigin, ""},
		{"whitespace only", []string{"   "}, v1.ErrNoOrigin, ""},
		{"unproxyable scheme", []string{"ftp://localhost:21"}, v1.ErrInvalidOrigin, "ftp"},
		{"scheme with no host", []string{"http://"}, v1.ErrInvalidOrigin, "http://"},
		{"one bad origin among good ones", []string{"http://localhost:3000", "ftp://localhost:21"}, v1.ErrInvalidOrigin, "ftp"},
		{"two origins claiming the websockets", []string{"http+ws://localhost:4000", "http+ws://localhost:5173"}, v1.ErrInvalidOrigin, "http+ws://localhost:5173"},
		{"the marker on a container", []string{"dockerd+ws://api"}, v1.ErrInvalidOrigin, "dockerd+ws"},
		{"the marker on an unproxyable scheme", []string{"ftp+ws://localhost:21"}, v1.ErrInvalidOrigin, "ftp"},
		{"a container with no name", []string{"dockerd://"}, v1.ErrInvalidOrigin, "dockerd://"},
		{"a container with a path", []string{"dockerd://api/sh"}, v1.ErrInvalidOrigin, "dockerd://api"},
		{"a container with a query", []string{"dockerd://api?tty=1"}, v1.ErrInvalidOrigin, "dockerd://api"},
		{"a container with a fragment", []string{"dockerd://api#sh"}, v1.ErrInvalidOrigin, "dockerd://api"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseOrigins(tc.in)
			if err == nil {
				t.Fatalf("parseOrigins(%q) = %v, want an error", tc.in, got)
			}
			if !errors.Is(err, tc.want) {
				t.Errorf("error = %v, want %v", err, tc.want)
			}
			if tc.mention != "" && !strings.Contains(err.Error(), tc.mention) {
				t.Errorf("error %q does not name %q", err, tc.mention)
			}
		})
	}
}

// TestPublicURL pins the routing contract: with more than one origin every
// address carries a bare ?i, the parameter the tunnel's proxy consumes — the
// default origin included, since a plain URL routes by referer and cookie and
// so stops reaching origin 0 once a browser has visited ?1. A valued parameter
// ("?1=x") would be application data and route nowhere, so the bareness is
// half the assertion and the explicit ?0 is the other half.
//
// A lone origin has nothing to route between and gets the plain URL. The
// tunnel URL itself must survive unmodified either way, since every later call
// derives from it.
func TestPublicURL(t *testing.T) {
	public, err := url.Parse("https://foo.tunneled.pizza/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cases := []struct {
		name string
		i, n int
		want string
	}{
		{"lone origin is plain", 0, 1, "https://foo.tunneled.pizza/"},
		{"default origin is explicit when it can be confused", 0, 2, "https://foo.tunneled.pizza/?0"},
		{"second origin", 1, 2, "https://foo.tunneled.pizza/?1"},
		{"double digits", 12, 13, "https://foo.tunneled.pizza/?12"},
	}
	for _, tc := range cases {
		if got := PublicURL(public, tc.i, tc.n); got != tc.want {
			t.Errorf("%s: PublicURL(_, %d, %d) = %q, want %q", tc.name, tc.i, tc.n, got, tc.want)
		}
	}
	if public.RawQuery != "" {
		t.Errorf("PublicURL mutated its argument: RawQuery = %q, want empty", public.RawQuery)
	}
}

// TestReportWritesOnlyToStderr pins the output contract: a running tunnel
// writes its addresses to stderr and nothing at all to stdout.
//
// stdout used to carry one bare URL per origin as a machine interface. It
// meant every address printed twice wherever both streams landed together,
// and the de-duplication meant to hide that could only recognise one file
// descriptor being literally the other — which a container's two pipes are
// not, so it never fired there. The map says which origin each address
// reaches, which the bare lines never did.
func TestReportWritesOnlyToStderr(t *testing.T) {
	public, err := url.Parse("https://foo.tunneled.pizza/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	origins, err := parseOrigins([]string{"http://localhost:3000", "http://localhost:4000"})
	if err != nil {
		t.Fatalf("parseOrigins: %v", err)
	}

	var stderr bytes.Buffer
	report(&stderr, public, origins, "")

	for _, want := range []string{
		"  https://foo.tunneled.pizza/?0\n    -> http://localhost:3000\n",
		"  https://foo.tunneled.pizza/?1\n    -> http://localhost:4000\n",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q does not contain %q", stderr.String(), want)
		}
	}

	// Every address appears once: in the map, and nowhere else.
	for _, addr := range []string{"https://foo.tunneled.pizza/?0", "https://foo.tunneled.pizza/?1"} {
		if got := strings.Count(stderr.String(), addr); got != 1 {
			t.Errorf("%s appears %d times, want 1:\n%s", addr, got, stderr.String())
		}
	}
}

// TestLogger covers the level resolution: the --log-level value wins, the
// environment mirror is the fallback, and neither being set means silence. The
// flag is strict (an operator typo must not vanish) while the environment is
// lenient, matching what the underlying library does with its own knob.
func TestLogger(t *testing.T) {
	cases := []struct {
		name     string
		level    string
		env      string
		wantErr  error
		enabled  slog.Level
		disabled bool // the level above must NOT be enabled
	}{
		{name: "unset is silent", enabled: slog.LevelError, disabled: true},
		{name: "flag sets the level", level: "debug", enabled: slog.LevelDebug},
		{name: "environment is the fallback", env: "debug", enabled: slog.LevelDebug},
		{name: "flag beats environment", level: "error", env: "debug", enabled: slog.LevelInfo, disabled: true},
		{name: "unparsable environment reads as info", env: "loud", enabled: slog.LevelInfo},
		{name: "unparsable flag is an error", level: "loud", wantErr: v1.ErrInvalidLogLevel},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(v1.LogEnv, tc.env)
			b := New()
			b.logLevel = tc.level

			log, err := b.logger()
			if tc.wantErr != nil {
				if !errors.Is(err, tc.wantErr) {
					t.Fatalf("error = %v, want %v", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("logger() = error %v, want ok", err)
			}
			if got := log.Enabled(t.Context(), tc.enabled); got == tc.disabled {
				t.Errorf("Enabled(%v) = %v, want %v", tc.enabled, got, !tc.disabled)
			}
		})
	}
}

// TestOpenInBrowser pins the two things the opener promises. It never writes
// to stdout — the spawned process inherits writers from pkg/browser's package
// globals, which default to os.Stdout, and that stream carries nothing but
// public URLs. And a failure to open is a warning, not an error: the tunnel is
// up and serving either way, and a headless host is a normal place to run
// this, not a broken one.
func TestOpenInBrowser(t *testing.T) {
	t.Run("opens the address and leaves stdout alone", func(t *testing.T) {
		var opened string
		swapOpener(t, func(addr string) error {
			opened = addr
			return nil
		})

		var stderr bytes.Buffer
		openInBrowser("https://foo.tunneled.pizza/", &stderr, slog.New(slog.DiscardHandler))

		if want := "https://foo.tunneled.pizza/"; opened != want {
			t.Errorf("opened %q, want %q", opened, want)
		}
		if browser.Stdout != io.Writer(&stderr) {
			t.Error("browser.Stdout was left pointing elsewhere, want the stderr writer")
		}
		if browser.Stderr != io.Writer(&stderr) {
			t.Error("browser.Stderr was left pointing elsewhere, want the stderr writer")
		}
	})

	// A headless host — a server, a container, CI — is a normal place to run
	// this, not a broken one, so the failure must not reach an operator who
	// never asked about it. Both halves matter: silent at warn, and still
	// there for somebody debugging a browser that did not appear.
	t.Run("a failure is quiet outside the debug log", func(t *testing.T) {
		swapOpener(t, func(string) error { return errors.New("no browser here") })

		var quiet bytes.Buffer
		openInBrowser("https://foo.tunneled.pizza/", io.Discard,
			slog.New(slog.NewTextHandler(&quiet, &slog.HandlerOptions{Level: slog.LevelWarn})))
		if quiet.Len() != 0 {
			t.Errorf("log = %q, want nothing at warn level", quiet.String())
		}

		var logged bytes.Buffer
		log := slog.New(slog.NewTextHandler(&logged, &slog.HandlerOptions{Level: slog.LevelDebug}))
		openInBrowser("https://foo.tunneled.pizza/", io.Discard, log)

		if !strings.Contains(logged.String(), "could not open a browser") {
			t.Errorf("log = %q, want the failure in the debug log", logged.String())
		}
	})
}

// swapOpener replaces the browser launcher for one test and restores it after,
// so the suite never opens a window on whoever is running it.
func swapOpener(t *testing.T, fn func(string) error) {
	t.Helper()
	old := openURL
	t.Cleanup(func() { openURL = old })
	openURL = fn
}

// TestReportNamesTheMultiviewPanel pins that the panel's own address is what
// stderr leads with when there is one: it answers for every origin at once, so
// the per-origin addresses become the indented list beneath it.
func TestReportNamesTheMultiviewPanel(t *testing.T) {
	public, err := url.Parse("https://foo.tunneled.pizza/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	origins, err := parseOrigins([]string{"http://localhost:3000", "http://localhost:4000"})
	if err != nil {
		t.Fatalf("parseOrigins: %v", err)
	}

	var stderr bytes.Buffer
	report(&stderr, public, origins, multiview.URL(public))

	if !strings.Contains(stderr.String(), "https://foo.tunneled.pizza/\n") {
		t.Errorf("stderr %q does not name the panel", stderr.String())
	}
	for _, origin := range []string{"-> http://localhost:3000", "-> http://localhost:4000"} {
		if !strings.Contains(stderr.String(), origin) {
			t.Errorf("stderr %q does not list %q under the panel", stderr.String(), origin)
		}
	}
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
