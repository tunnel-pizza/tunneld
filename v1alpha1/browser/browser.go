// Package browser puts a public address in front of a person: it launches
// whatever browser the host has. Its own subpackage because pkg/browser is a
// world the root has no reason to see.
package browser

import (
	"context"
	"io"

	pkgbrowser "github.com/pkg/browser"
	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Option configures a BrowserImpl at construction.
type Option = v1.Option[*BrowserImpl]

// BrowserImpl is the default opener. Its launcher is seeded by New; a bare
// BrowserImpl{} has none and is not a supported construction.
type BrowserImpl struct {
	launch func(string) error
}

// New returns an opener that launches the host's browser, then configured by
// opts.
func New(opts ...Option) *BrowserImpl {
	b := v1.Apply(&BrowserImpl{}, WithLaunch(pkgbrowser.OpenURL))
	return v1.Apply(b, opts...)
}

// WithLaunch replaces the browser launcher, so a test can observe the call
// without a window appearing on whoever is running it.
func WithLaunch(launch func(string) error) Option {
	return func(b *BrowserImpl) { b.launch = launch }
}

// Open launches a browser on addr. It does not wait, and the context is
// accepted only because the contract carries one.
//
// Waiting for the edge to serve addr is no longer this package's job. It used
// to probe over HTTP, which asked the right question from the wrong place: the
// tunnel's own event stream already knows when the edge has accepted a
// connection, so the counter holds that answer and the caller waits there
// before calling this. What is left is the launch, and the two promises around
// it.
//
// A failure goes to the debug log and nowhere else: the tunnel is up and
// serving either way, and a headless host — a server, a container, CI — is a
// normal place to run this, not a broken one. A warning on stderr told those
// runs, every time, about a thing they were never going to do. --no-open (or
// v1.NoOpenEnv) skips the attempt entirely, and --log-level=debug is where to
// look when a browser was wanted and none appeared.
//
// pkg/browser wires the spawned process's output to its package-level Stdout,
// which defaults to os.Stdout — the stream a running tunnel keeps for its
// addresses. Both are pointed at stderr before the child can write a word.
// They are package globals, so this is process-wide; tunneld owns its process,
// and an embedding program gets the same guarantee it wants anyway.
func (b *BrowserImpl) Open(_ context.Context, addr string, stderr io.Writer, log v1.Logger) {
	pkgbrowser.Stdout, pkgbrowser.Stderr = stderr, stderr
	if err := b.launch(addr); err != nil {
		log.Debug("could not open a browser", "url", addr, "error", err)
	}
}
