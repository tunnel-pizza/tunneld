// Command basic is the smallest complete tunneld example: it starts a real
// local service, exposes it to the public internet, opens the public URL in a
// browser, and blocks until interrupted.
//
// Nothing else needs to be running — the origin on :3000 is this program's
// own. Every tunneld flag still works, since the seeded origin is only a
// default:
//
//	go run ./examples/basic --url http://localhost:8080 --open=false
package main

import (
	"context"
	"errors"
	"fmt"
	"html"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"
	"github.com/tunnel-pizza/tunneld/lib"
)

func main() {
	// The context is the tunnel's shutdown handle and the origin's: Ctrl-C
	// tears both down, whether it arrives during startup or after the URL is
	// live.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := lib.New().
		WithURL("http://localhost:3000").
		Build()

	// Build returns an ordinary *cobra.Command, so the origin hangs off its
	// PreRunE. That hook rather than plain code before ExecuteContext because
	// cobra answers --help before it runs, so `--help` stays a pure question:
	// it prints and exits without binding a port.
	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		return serve(cmd.Context(), ":3000", "basic")
	}

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "basic: "+err.Error())
		os.Exit(1)
	}
}

// serve starts an HTTP server on addr and returns once it is listening, so the
// tunnel never proxies to a socket that is not up yet. It shuts down with ctx.
func serve(ctx context.Context, addr, name string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}

	srv := &http.Server{
		Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			// The request target is whatever the visitor typed, and it lands
			// in an HTML document — escape it, or the echo below is a
			// reflected-XSS hole in a file people copy as a starter.
			fmt.Fprintf(w, page, name, addr, html.EscapeString(r.URL.RequestURI()))
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

// page is what a visitor sees. It echoes the request so the tunnel's work is
// visible from the browser: the path arrives at the origin unchanged.
const page = `<!doctype html>
<meta charset="utf-8">
<title>%[1]s</title>
<h1>%[1]s</h1>
<p>served locally by <code>%[2]s</code>, reached through a tunnel</p>
<p>you asked for <code>%[3]s</code></p>
`
