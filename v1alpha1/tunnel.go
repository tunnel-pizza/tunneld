package v1alpha1

import (
	"cmp"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/cnuss/libtunnel"
	"github.com/pkg/browser"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// openURL launches a browser on addr. A variable, not a direct call, so a test
// can observe the call without a window appearing on whoever is running it.
var openURL = browser.OpenURL

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

	// Served in front of the origin proxy, so the panel needs no port of its
	// own and no origin ever sees the request.
	view := ""
	if wantsMultiview(b.multiview, origins) {
		tun.WithInterceptor(multiview(origins, log))
		tun.WithInterceptor(unframe())
	}

	log.Info("tunneld starting", "version", Version(), "libtunnel", libtunnel.Version(), "origins", len(origins))

	public := tun.URL()
	if public == nil {
		return cmp.Or(tun.Err(), ctx.Err(), v1.ErrNotReady)
	}
	if wantsMultiview(b.multiview, origins) {
		view = multiviewURL(public)
	}
	report(stdout, stderr, public, origins, view)
	if b.open {
		// One page, never a fan of tabs: the panel when there is one, since it
		// reaches every origin, and otherwise the default origin itself.
		openInBrowser(cmp.Or(view, PublicURL(public, 0, len(origins))), stderr, log)
	}

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
//
// view, the panel's address when there is one, is named on stderr only. It
// reaches every origin rather than one, so putting it on stdout would break
// the line-i-is-origin-i rule that makes that stream worth reading.
//
// Unless the two streams land in the same place, which on a terminal they
// normally do — and there the split is invisible, so every URL would simply
// appear twice, once bare and once in the map. When they do, the bare lines
// are dropped: the map already shows every address, in a form that says which
// origin it reaches. Redirect either stream and both come back, because then
// they are going somewhere different and the machine-readable one has a reader.
func report(stdout, stderr io.Writer, public *url.URL, origins []*url.URL, view string) {
	fmt.Fprintln(stderr, VersionLine())
	merged := sameStream(stdout, stderr)
	width := len(view)
	for i := range origins {
		width = max(width, len(PublicURL(public, i, len(origins))))
	}
	for i, origin := range origins {
		addr := PublicURL(public, i, len(origins))
		if !merged {
			fmt.Fprintln(stdout, addr)
		}
		fmt.Fprintf(stderr, "  %-*s -> %s\n", width, addr, origin)
	}
	if view != "" {
		fmt.Fprintf(stderr, "  %-*s -> all %d, framed together\n", width, view, len(origins))
	}
}

// sameStream reports whether two writers end up at the same terminal, file, or
// pipe. Anything that is not an *os.File — a buffer in a test, a writer an
// embedding program supplied — is never merged: it has a reader of its own by
// construction.
//
// Stat rather than an isatty check, because the case is not "is this a
// terminal" but "would the reader see this twice", which is equally true of
// `tunneld --url ... >out 2>&1`.
func sameStream(a, b io.Writer) bool {
	af, ok := a.(*os.File)
	if !ok {
		return false
	}
	bf, ok := b.(*os.File)
	if !ok {
		return false
	}
	ai, err := af.Stat()
	if err != nil {
		return false
	}
	bi, err := bf.Stat()
	if err != nil {
		return false
	}
	return os.SameFile(ai, bi)
}

// openInBrowser launches a browser on addr, reporting a failure as a warning
// rather than an error: the tunnel is up and serving either way, and a
// headless host — a server, a container, CI — is a normal place to run this,
// not a broken one. --open=false (or v1.OpenEnv) turns off the attempt.
//
// pkg/browser wires the spawned process's output to its package-level Stdout,
// which defaults to os.Stdout — the one stream tunneld promises carries
// nothing but public URLs. Both are pointed at stderr before the child can
// write a word. They are package globals, so this is process-wide; tunneld
// owns its process, and an embedding program gets the same guarantee it wants
// anyway.
func openInBrowser(addr string, stderr io.Writer, log *slog.Logger) {
	browser.Stdout, browser.Stderr = stderr, stderr
	if err := openURL(addr); err != nil {
		log.Warn("could not open a browser", "url", addr, "error", err)
	}
}

// PublicURL is the address origin i answers on, out of n origins: the tunnel's
// URL with a bare ?i routing parameter. Bare is load-bearing — a valued
// parameter ("?1=x") is application data the proxy forwards, while the bare
// form is the routing directive it consumes and strips before the request
// reaches the origin.
//
// The default origin is explicit too, as ?0, whenever there is more than one.
// A bare URL routes by the referring page and then by the sticky cookie, so
// once a browser has visited ?1 a plain address no longer reaches origin 0 —
// only an explicit index clears a previous choice. An address that stops
// working after someone clicks around is worse than a longer one.
//
// A lone origin has nothing to route between, so n of 1 gives the plain URL
// and no parameter at all.
func PublicURL(public *url.URL, i, n int) string {
	if n <= 1 {
		return public.String()
	}
	routed := *public
	routed.RawQuery = strconv.Itoa(i)
	return routed.String()
}

// parseOrigins turns the settled origin values into URLs, rejecting anything
// the tunnel could not proxy to.
//
// Two shorthands are filled in, both of them what people actually type: a
// value with no scheme implies http, and a value with no host implies
// localhost, so ":8000" and "localhost:8000" and "http://localhost:8000" are
// one origin written three ways. Everything else must carry an http or https
// scheme and a host, so a typo surfaces here rather than as a public hostname
// that answers only errors. Every failure wraps a v1 sentinel and names the
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
		// A port with no host in front of it — ":8000", or the
		// "http://:8000" the scheme default above makes of it — means the
		// local machine, the way every dev server reads that shorthand. The
		// test is Hostname, not Host: url.Parse keeps the colon, so ":8000"
		// arrives as a non-empty Host with nothing before the port, and a
		// bare Host check waves it through as the unresolvable origin
		// "http://:8000".
		if u.Hostname() == "" {
			if u.Port() == "" {
				return nil, fmt.Errorf("%w: %q has no host, pass e.g. http://localhost:3000", v1.ErrInvalidOrigin, s)
			}
			u.Host = net.JoinHostPort("localhost", u.Port())
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
