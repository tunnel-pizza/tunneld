// Command multi-origin exposes two local services through a single tunnel.
//
// They share one public hostname: the first --url is the default and the
// second answers on a bare ?1 parameter, so a frontend on :3000 and an API on
// :4000 live behind one name without a tunnel per port.
//
//	https://<host>/     -> http://localhost:3000
//	https://<host>/?1   -> http://localhost:4000
//
// A browser sticks to whichever origin it landed on — subresources follow
// their document's URL, and a top-level visit to ?1 is remembered by cookie.
//
// It needs something listening on :3000 and :4000.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tunnel-pizza/tunneld/lib"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := lib.New().
		WithURL("http://localhost:3000", "http://localhost:4000").
		// Two origins but one browser tab would open, on the default origin.
		// Off here so the example reports the whole map and opens nothing.
		WithOpen(false).
		Build()

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "multi-origin: "+err.Error())
		os.Exit(1)
	}
}
