package v1alpha1

import (
	"context"
	"io"
	"log/slog"
	"net/url"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/docker"
)

// openTarget resolves a container reference. A variable, not a direct call, so
// a test can bind origins without a Docker daemon — the ordering invariant
// below is the part worth pinning, and it has nothing to do with containers.
var openTarget = func(ctx context.Context, ref string, log *slog.Logger) (attach.Target, error) {
	return docker.Open(ctx, ref, log)
}

// bound is the result of binding: the list libtunnel dials, and the servers
// that have to be shut down with it.
type bound struct {
	dialable []*url.URL
	closers  []io.Closer
}

// bindOrigins turns the origins the operator typed into the origins libtunnel
// can proxy to.
//
// An http or https origin passes through untouched; a dockerd:// origin is
// served here, by a loopback attach server that takes its place in the list.
// The two lists share a length and an order, which is the whole point: index n
// still means origin n for the bare ?n routing parameter, for PublicURL, for
// the reported map and for the multiview tiles, so a container is an origin
// like any other and nothing downstream learns a second shape.
//
// A failure unwinds everything already bound. The command is about to return
// an error, and a listener left behind would outlive it inside an embedding
// program.
func bindOrigins(ctx context.Context, display []*url.URL, log *slog.Logger) (*bound, error) {
	b := &bound{dialable: make([]*url.URL, 0, len(display))}
	for _, origin := range display {
		if origin.Scheme != v1.DockerScheme {
			b.dialable = append(b.dialable, origin)
			continue
		}

		target, err := openTarget(ctx, origin.Host, log)
		if err != nil {
			_ = b.Close()
			return nil, err
		}
		server, err := attach.Serve(ctx, target, log)
		if err != nil {
			_ = target.Close()
			_ = b.Close()
			return nil, err
		}
		b.closers = append(b.closers, server)
		b.dialable = append(b.dialable, server.URL())
		log.Info("serving a container as an origin", "container", origin.Host, "origin", server.URL())
	}
	return b, nil
}

// Close shuts every attach server down, and with it every container client
// they own. The first error is returned and the rest still close: a partial
// shutdown is worse than a lost error message.
func (b *bound) Close() error {
	var err error
	for _, closer := range b.closers {
		if cerr := closer.Close(); err == nil {
			err = cerr
		}
	}
	return err
}
