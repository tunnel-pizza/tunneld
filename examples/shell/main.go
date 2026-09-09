// Command shell exposes a local program as a browser terminal: it seeds a
// program origin, opens the public URL in a browser, and blocks until
// interrupted.
//
// Nothing else needs to be running and nothing is proxied — k9s is started by
// tunneld itself, on a pseudo-terminal, when the first viewer opens the page.
// A bare word this machine can run is a program origin, so the seed is the
// command as somebody would type it; what the frame shows is the path it
// resolved to.
//
// Every tunneld flag still works, and so does any other program, since the
// seeded origin is only a default:
//
//	go run ./examples/shell top --no-open
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tunnel-pizza/tunneld/v1alpha1"
)

func main() {
	// The context is the tunnel's shutdown handle and the origin's: Ctrl-C
	// tears both down, whether it arrives during startup or after the URL is
	// live.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := v1alpha1.New(
		v1alpha1.WithOrigin("k9s"),
		v1alpha1.WithLogLevel("debug"),
	).Command()

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "shell: "+err.Error())
		os.Exit(1)
	}
}
