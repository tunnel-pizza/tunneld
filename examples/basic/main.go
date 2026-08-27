// Command basic is the smallest tunneld example: expose one already-running
// local service to the public internet and block until interrupted.
//
// It needs something listening on :3000. Point it elsewhere without editing —
// the seeded origin is a default, so `go run . --url http://localhost:8080`
// replaces it, and every other tunneld flag works here too.
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
	// The context is the tunnel's shutdown handle: Ctrl-C tears it down
	// whether it arrives during startup or after the URL is live.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := lib.New().
		WithURL("http://localhost:3000").
		Build()

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "basic: "+err.Error())
		os.Exit(1)
	}
}
