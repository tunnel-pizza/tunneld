package v1alpha1

import (
	"bytes"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/panel"
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
	report(&stderr, public, origins, panel.New().URL(public))

	if !strings.Contains(stderr.String(), "https://foo.tunneled.pizza/\n") {
		t.Errorf("stderr %q does not name the panel", stderr.String())
	}
	for _, origin := range []string{"-> http://localhost:3000", "-> http://localhost:4000"} {
		if !strings.Contains(stderr.String(), origin) {
			t.Errorf("stderr %q does not list %q under the panel", stderr.String(), origin)
		}
	}
}
