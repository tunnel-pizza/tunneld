// Command attach exposes a running container's terminal through a tunnel.
//
// Unlike the other examples it cannot start what it exposes: a container is
// somebody else's process, which is the whole point of attaching to one rather
// than spawning it. Start one first:
//
//	docker run -d --rm --name tunneld-demo -it alpine sh
//
// then run this, and the public URL is a terminal in that container:
//
//	https://<host>/   -> dockerd://tunneld-demo
//
// Stop the container and the page says so. Ctrl-C in the container's shell
// stops the container, exactly as `docker attach` would — the signal reaches
// PID 1, because with a TTY that is what a terminal does.
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
		WithURL("dockerd://tunneld-demo").
		Build()

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "attach: "+err.Error())
		os.Exit(1)
	}
}
