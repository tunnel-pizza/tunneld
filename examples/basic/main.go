// Command basic is the smallest complete tunneld example: it starts a real
// local service, exposes it to the public internet, opens the public URL in a
// browser, and blocks until interrupted.
//
// Nothing else needs to be running — the origin on :3000 is this program's
// own, serving the same page examples/multi-origin puts in its first tile. Every tunneld flag still works, since the seeded origin is only a
// default:
//
//	go run ./examples/basic --url http://localhost:8080 --no-open
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
	// The context is the tunnel's shutdown handle and the origin's: Ctrl-C
	// tears both down, whether it arrives during startup or after the URL is
	// live.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := v1alpha1.New().
		WithURL("http://localhost:3000").
		Build()

	// Build returns an ordinary *cobra.Command, so the origin hangs off its
	// PreRunE. That hook rather than plain code before ExecuteContext because
	// cobra answers --help before it runs, so `--help` stays a pure question:
	// it prints and exits without binding a port.
	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		return serve(cmd.Context(), ":3000", "frontend.html")
	}

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "basic: "+err.Error())
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
			fmt.Fprintln(os.Stderr, "basic: "+err.Error())
		}
	}()
	return nil
}
