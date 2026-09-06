// Package browser puts a public address in front of a person: it waits for
// the edge to actually serve the address, then launches whatever browser the
// host has. Its own subpackage because pkg/browser and an HTTP probe are a
// world the root has no reason to see.
package browser

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"time"

	pkgbrowser "github.com/pkg/browser"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// The window between a tunnel being ready and the edge serving it. Ten seconds
// is far longer than the gap has been observed to be — under a second — and it
// only ever costs that much when something is wrong, in which case the browser
// opens anyway rather than never.
const (
	reachableWithin = 10 * time.Second
	reachableEvery  = 250 * time.Millisecond
	reachableProbe  = 5 * time.Second
)

// Option configures an OpenerImpl at construction.
type Option = v1.Option[*OpenerImpl]

// OpenerImpl is the default opener. Both fields are seeded by New; a bare
// OpenerImpl{} has no launcher and is not a supported construction.
type OpenerImpl struct {
	launch func(string) error
	window time.Duration
}

// New returns an opener that probes for ten seconds and launches the host's
// browser, then configured by opts.
func New(opts ...Option) *OpenerImpl {
	o := v1.Apply(&OpenerImpl{}, WithLaunch(pkgbrowser.OpenURL), WithWindow(reachableWithin))
	return v1.Apply(o, opts...)
}

// WithLaunch replaces the browser launcher, so a test can observe the call
// without a window appearing on whoever is running it.
func WithLaunch(launch func(string) error) Option {
	return func(o *OpenerImpl) { o.launch = launch }
}

// WithWindow bounds how long Open waits for the edge before launching anyway.
func WithWindow(d time.Duration) Option {
	return func(o *OpenerImpl) { o.window = d }
}

// Open waits for the edge to serve addr, then launches a browser on it. The
// wait is bounded by the window; the launch happens either way, since a page
// that may work beats no page at all.
func (o *OpenerImpl) Open(ctx context.Context, addr string, stderr io.Writer, log v1.Logger) {
	awaitReachable(ctx, addr, o.window, log)
	openInBrowser(o.launch, addr, stderr, log)
}

// awaitReachable waits for the edge to actually serve addr before a browser is
// pointed at it.
//
// TunnelReady, which URL has already waited on, means the connection is up and
// the hostname resolves. It does not mean the edge has finished registering
// the route: for a moment after that it answers 530, and a browser opened into
// that window shows an error page for a tunnel that is about to work. Measured
// at roughly half a second, which is exactly long enough to be the first thing
// somebody sees.
//
// Anything the origin itself produced ends the wait, 404 and 401 included —
// the question is whether the route is live, not whether the app is happy. A
// 5xx is the edge saying it still cannot reach the tunnel. Giving up opens the
// browser regardless: a page that may work beats no page at all, and the
// warning says which happened.
func awaitReachable(ctx context.Context, addr string, within time.Duration, log *slog.Logger) {
	ctx, cancel := context.WithTimeout(ctx, within)
	defer cancel()

	client := &http.Client{Timeout: reachableProbe}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodHead, addr, nil)
		if err != nil {
			return // a URL this far in is well-formed; nothing to retry
		}
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode < http.StatusInternalServerError {
				return
			}
			log.Debug("edge not serving yet", "url", addr, "status", resp.StatusCode)
		}

		select {
		case <-ctx.Done():
			log.Debug("opening a browser before the edge answered", "url", addr)
			return
		case <-time.After(reachableEvery):
		}
	}
}

// openInBrowser launches a browser on addr, reporting a failure to the debug
// log and nowhere else: the tunnel is up and serving either way, and a
// headless host — a server, a container, CI — is a normal place to run this,
// not a broken one. A warning on stderr told those runs, every time, about a
// thing they were never going to do. --no-open (or v1.NoOpenEnv) skips the
// attempt entirely, and --log-level=debug is where to look when a browser was
// wanted and none appeared.
//
// pkg/browser wires the spawned process's output to its package-level Stdout,
// which defaults to os.Stdout — the one stream tunneld promises carries
// nothing a running tunnel writes. Both are pointed at stderr before the
// child can write a word. They are package globals, so this is process-wide;
// tunneld owns its process, and an embedding program gets the same guarantee
// it wants anyway.
func openInBrowser(launch func(string) error, addr string, stderr io.Writer, log *slog.Logger) {
	pkgbrowser.Stdout, pkgbrowser.Stderr = stderr, stderr
	if err := launch(addr); err != nil {
		log.Debug("could not open a browser", "url", addr, "error", err)
	}
}
