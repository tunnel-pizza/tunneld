// Command tunneld exposes one or more already-running local origins to the
// public internet through a quick tunnel, in-process — no `cloudflared`
// binary, no account, no DNS to configure.
//
//	tunneld http://localhost:3000 http://localhost:4000
//
// It is a lite `cloudflared tunnel --url` with the multi-origin extension:
// every origin after the first is reachable under the same public hostname via
// a bare ?n query parameter, n being that argument's 0-based position.
//
// This package is the process shell and nothing more: it turns signals into a
// context and hands that to the command the builder assembles. The command
// itself — flags, help, the tunnel it runs — comes from
// [github.com/tunnel-pizza/tunneld/v1alpha1], which is also how another
// program embeds tunneld as a subcommand of its own.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/tunnel-pizza/tunneld/v1alpha1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/pid"
)

func main() {
	// The signal context is the tunnel's shutdown handle, so Ctrl-C tears the
	// tunnel down whether it arrives during startup or after the URL is live.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// SilenceErrors is set on the built command, so the error surfaces here
	// exactly once, prefixed with the program name. Being found and handed
	// back by the npm launcher is this binary's, not every program's that
	// embeds the builder: it is how npx tunneld -k and -d reach a run.
	if err := v1alpha1.New(v1alpha1.WithPid(pid.New())).Command().ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "tunneld: "+err.Error())
		os.Exit(1)
	}
}
