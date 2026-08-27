// Command multi-origin exposes two local services through a single tunnel.
//
// It starts both — nothing else needs to be running — and puts them behind one
// public hostname. The first --url is the default; the second answers on a
// bare ?1 parameter, so a frontend on :3000 and an API on :4000 share a name
// without a tunnel per port:
//
//	https://<host>/?0   -> http://localhost:3000
//	https://<host>/?1   -> http://localhost:4000
//
// The parameter is a routing directive the tunnel consumes; it never reaches
// the origin, which is why each page below can echo the path it was asked for
// and show it arriving clean. A browser then sticks to whichever origin it
// landed on — subresources follow their document's URL, and a top-level visit
// to ?1 is remembered by cookie.
//
// That stickiness is why the pages link to ?0 rather than to "/" for the
// default origin: only an explicit index clears a previous choice.
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := lib.New().
		WithURL("http://localhost:3000", "http://localhost:4000").
		// Only the default origin would open, and this example is about
		// seeing both. Off, so it reports the whole map and opens nothing.
		WithOpen(false).
		Build()

	// PreRunE rather than plain code before ExecuteContext: cobra answers
	// --help before that hook runs, so `--help` stays a pure question and
	// binds no ports.
	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		if err := serve(cmd.Context(), ":3000", "frontend"); err != nil {
			return err
		}
		return serve(cmd.Context(), ":4000", "api")
	}

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "multi-origin: "+err.Error())
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
			fmt.Fprintln(os.Stderr, "multi-origin: "+err.Error())
		}
	}()
	return nil
}

// page names which origin answered, so switching between the two links in a
// browser shows the routing working. It echoes the request path too: the
// routing parameter is gone by the time it arrives here.
//
// Both links carry an explicit parameter, ?0 included. A bare "/" would not
// come back to the default origin once you had visited ?1 — with no parameter
// of its own the tunnel routes by the referring page's, and the sticky cookie
// still names origin 1 besides. ?0 is what says "this one, and forget the last
// choice": an explicit index routes and rewrites the cookie in one move.
const page = `<!doctype html>
<meta charset="utf-8">
<title>%[1]s</title>
<h1>%[1]s</h1>
<p>served locally by <code>%[2]s</code>, reached through a tunnel</p>
<p>you asked for <code>%[3]s</code></p>
<p><a href="/?0">origin 0</a> &middot; <a href="/?1">origin 1</a></p>
`
