package v1alpha1

import (
	"bytes"
	"errors"
	"log/slog"
	"net/url"
	"strings"
	"testing"

	v1 "github.com/tunnel-pizza/tunneld/v1"
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
		{"surrounding space trimmed", []string{"  http://localhost:3000  "}, []string{"http://localhost:3000"}},
		{"path preserved", []string{"http://localhost:3000/api"}, []string{"http://localhost:3000/api"}},
		{
			"order preserved across origins",
			[]string{"http://localhost:3000", "http://localhost:4000"},
			[]string{"http://localhost:3000", "http://localhost:4000"},
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

// TestPublicURL pins the routing contract: origin 0 answers on the tunnel URL
// untouched, and origin i answers on that URL with a bare ?i — the parameter
// the tunnel's proxy consumes. A valued parameter ("?1=x") would be application
// data and route nowhere, so the bareness is the assertion. The tunnel URL
// itself must survive unmodified, since every later call derives from it.
func TestPublicURL(t *testing.T) {
	public, err := url.Parse("https://foo.tunneled.pizza/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	cases := []struct {
		i    int
		want string
	}{
		{0, "https://foo.tunneled.pizza/"},
		{1, "https://foo.tunneled.pizza/?1"},
		{12, "https://foo.tunneled.pizza/?12"},
	}
	for _, tc := range cases {
		if got := PublicURL(public, tc.i); got != tc.want {
			t.Errorf("PublicURL(_, %d) = %q, want %q", tc.i, got, tc.want)
		}
	}
	if public.RawQuery != "" {
		t.Errorf("PublicURL mutated its argument: RawQuery = %q, want empty", public.RawQuery)
	}
}

// TestReportSplitsStreams pins the output contract: stdout is one public URL
// per origin in order and nothing else, so `| head -1` reaches the default
// origin and line i reaches origin i. Everything human — the banner, the
// arrows — belongs to stderr, where it cannot corrupt that stream.
func TestReportSplitsStreams(t *testing.T) {
	public, err := url.Parse("https://foo.tunneled.pizza/")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	origins, err := parseOrigins([]string{"http://localhost:3000", "http://localhost:4000"})
	if err != nil {
		t.Fatalf("parseOrigins: %v", err)
	}

	var stdout, stderr bytes.Buffer
	report(&stdout, &stderr, public, origins)

	wantOut := "https://foo.tunneled.pizza/\nhttps://foo.tunneled.pizza/?1\n"
	if stdout.String() != wantOut {
		t.Errorf("stdout = %q, want %q", stdout.String(), wantOut)
	}
	for _, want := range []string{
		VersionLine(),
		"https://foo.tunneled.pizza/?1 -> http://localhost:4000",
	} {
		if !strings.Contains(stderr.String(), want) {
			t.Errorf("stderr %q does not contain %q", stderr.String(), want)
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
