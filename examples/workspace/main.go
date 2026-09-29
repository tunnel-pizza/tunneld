// Command workspace exposes a small development workspace through one tunnel:
// a Streamlit app, the terminal of the process serving it, and a Claude Code
// session, side by side in the multiview panel.
//
// The app and the session are both program origins, started by tunneld itself
// on pseudo-terminals — uvx runs Streamlit's demo on :8501, which the first
// origin proxies, and claude runs in this directory. So uvx and claude need to
// be installed; nothing else needs to be running.
//
// It is also the example to try the experimental Trellis panel with: TRELLIS=1
// in the environment, or the commented line below, serves it in place of the
// regular one.
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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// os.Setenv("TRELLIS", "1")
	cmd := v1alpha1.New(
		v1alpha1.WithOrigin("http://localhost:8501", "uvx streamlit hello --server.headless true", "claude"),
		v1alpha1.WithOpen(false),
	).Command()

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "workspace: "+err.Error())
		os.Exit(1)
	}
}
