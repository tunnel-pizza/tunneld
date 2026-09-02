// Command multi-origin exposes two local services through a single tunnel.
//
// It starts both — nothing else needs to be running — and puts them behind one
// public hostname, so a frontend on :3000 and an API on :4000 share a name
// without a tunnel per port:
//
//	https://<host>/     -> both, framed side by side
//	https://<host>/?0   -> http://localhost:3000
//	https://<host>/?1   -> http://localhost:4000
//
// The index is a routing directive the tunnel consumes; it never reaches the
// origin. The bare address belongs to the panel, which is why each origin has
// an index of its own — and why reaching one with query parameters of its own
// means keeping the index alongside them, as /?0&page=1.
//
// Run it with --multiview=false to hand the bare address back to the default
// origin and see the same two services without the panel.
package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/tunnel-pizza/tunneld/examples/sites"
	"github.com/tunnel-pizza/tunneld/v1alpha1"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := v1alpha1.New().
		WithURL("http://localhost:3000", "http://localhost:4000").
		// Only the default origin would open, and this example is about
		// seeing both. Off, so it reports the whole map and opens nothing.
		WithOpen(false).
		Build()

	// PreRunE rather than plain code before ExecuteContext: cobra answers
	// --help before that hook runs, so `--help` stays a pure question and
	// binds no ports.
	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		if err := serve(cmd.Context(), ":3000", "frontend.html"); err != nil {
			return err
		}
		return serve(cmd.Context(), ":4000", "api.html")
	}

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "multi-origin: "+err.Error())
		os.Exit(1)
	}
}

// serve starts an HTTP server on addr answering with the named page, and
// returns once it is listening, so the tunnel never proxies to a socket that is
// not up yet. It shuts down with ctx.
//
// Every path gets the same page: this is a stand-in for a real service, and a
// router would only be scenery.
func serve(ctx context.Context, addr, name string) error {
	page, err := sites.FS.ReadFile(name)
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write(page)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	context.AfterFunc(ctx, func() { _ = srv.Close() })

	go func() {
		if err := srv.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			fmt.Fprintln(os.Stderr, "multi-origin: "+err.Error())
		}
	}()
	return nil
}
