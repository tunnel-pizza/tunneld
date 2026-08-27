package v1alpha1

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/cnuss/libtunnel"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// run is the built command's body: it brings the tunnel up, reports the public
// URLs, and blocks until ctx is canceled or the tunnel fails. ctx is the
// shutdown handle — canceling it (a signal, in the binary's case) tears the
// tunnel down during startup as well as after, so this returns rather than
// hanging. A tunnel that fails on its own returns the cause.
//
// stdout and stderr come from the command's own OutOrStdout/ErrOrStderr, so
// cobra stays the single owner of where output goes; they are never nil.
//
// The engine is github.com/cnuss/libtunnel driving Cloudflare's edge in
// process — no cloudflared binary, no account, no DNS to configure.
func (b *BuilderImpl) run(ctx context.Context, stdout, stderr io.Writer) error {
	origins, err := parseOrigins(b.urls)
	if err != nil {
		return err
	}
	log, err := b.logger()
	if err != nil {
		return err
	}

	backend := libtunnel.Cloudflare()
	if b.provider != "" {
		backend = backend.WithProvider(b.provider)
	}
	// Pure-lazy: nothing dials until URL below trips the start. WithContext
	// upgrades URL from "the hostname resolves" to "reachable end to end" and
	// makes it return nil on cancel, so a signal during startup exits cleanly.
	tun := libtunnel.New(backend).
		WithLogger(log).
		WithContext(ctx).
		WithLocalURL(origins...)

	log.Info("tunneld starting", "version", Version(), "libtunnel", libtunnel.Version(), "origins", len(origins))

	public := tun.URL()
	if public == nil {
		return cmp.Or(tun.Err(), ctx.Err(), v1.ErrNotReady)
	}
	report(stdout, stderr, public, origins)

	select {
	case <-ctx.Done():
		return nil // signaled after the tunnel came up: clean shutdown
	case <-tun.Done():
		return tun.Err()
	}
}

// report writes the public URLs to stdout, one per origin in order, and the
// human-readable map to stderr. The split is what keeps the stdout stream
// machine-consumable: a script reads line i to reach origin i, while the
// banner and the arrows stay out of its way.
func report(stdout, stderr io.Writer, public *url.URL, origins []*url.URL) {
	fmt.Fprintln(stderr, VersionLine())
	width := 0
	for i := range origins {
		width = max(width, len(PublicURL(public, i)))
	}
	for i, origin := range origins {
		addr := PublicURL(public, i)
		fmt.Fprintln(stdout, addr)
		fmt.Fprintf(stderr, "  %-*s -> %s\n", width, addr, origin)
	}
}

// PublicURL is the address origin i answers on: the tunnel's URL itself for
// the default origin, and that URL with a bare ?i routing parameter for the
// rest. Bare is load-bearing — a valued parameter ("?1=x") is application data
// the proxy forwards, while the bare form is the routing directive it consumes
// and strips before the request reaches the origin.
func PublicURL(public *url.URL, i int) string {
	if i == 0 {
		return public.String()
	}
	routed := *public
	routed.RawQuery = strconv.Itoa(i)
	return routed.String()
}

// parseOrigins turns the settled origin values into URLs, rejecting anything
// the tunnel could not proxy to. A bare host:port (or host) implies http,
// matching what people type; anything else must carry an http or https scheme
// and a host, so a typo surfaces here rather than as a public hostname that
// answers only errors. Every failure wraps a v1 sentinel and names the
// offending value.
//
// The messages name the value, not the flag, because by this point a value may
// have arrived either way — through --url or through v1.URLEnv bound onto it.
// Only the nothing-at-all case names both, since that is the one an operator
// fixes by choosing between them.
func parseOrigins(raw []string) ([]*url.URL, error) {
	origins := make([]*url.URL, 0, len(raw))
	for _, s := range raw {
		s = strings.TrimSpace(s)
		if s == "" {
			return nil, fmt.Errorf("%w: empty origin, pass a local service URL (e.g. http://localhost:3000)", v1.ErrNoOrigin)
		}
		if !strings.Contains(s, "://") {
			s = "http://" + s
		}
		u, err := url.Parse(s)
		if err != nil {
			return nil, fmt.Errorf("%w: %q is not a URL: %w", v1.ErrInvalidOrigin, s, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("%w: %q has scheme %q, only http and https origins can be proxied", v1.ErrInvalidOrigin, s, u.Scheme)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("%w: %q has no host, pass e.g. http://localhost:3000", v1.ErrInvalidOrigin, s)
		}
		origins = append(origins, u)
	}
	if len(origins) == 0 {
		return nil, fmt.Errorf("%w: pass --url (or $%s) with the local service URL (e.g. http://localhost:3000)", v1.ErrNoOrigin, v1.URLEnv)
	}
	return origins, nil
}

// logger resolves the tunnel's log sink from the level the command settled on
// — the --log-level flag, or v1.LogEnv bound onto it by applyEnv. An
// unrecognized level is an error either way: somebody typed it, and a silent
// downgrade to info would hide the typo. Logger is the fallback when neither
// was set, which is silence.
//
// The sink is stderr either way, so logs never pollute the machine-readable
// URLs on stdout. run passes it to WithLogger, so tunneld's own startup line
// and the tunnel's internals share one logger and one level.
func (b *BuilderImpl) logger() (*slog.Logger, error) {
	if b.logLevel == "" {
		return Logger(), nil
	}
	var level slog.Level
	if err := level.UnmarshalText([]byte(b.logLevel)); err != nil {
		return nil, fmt.Errorf("%w: %q, want debug, info, warn or error (--log-level or $%s)", v1.ErrInvalidLogLevel, b.logLevel, v1.LogEnv)
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})), nil
}
