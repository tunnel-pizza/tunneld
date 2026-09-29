// Command noshell runs tunneld the way a machine with no shell of its own does
// — a minimal container image, a scrubbed environment — and so exposes the
// shell built into tunneld as a browser terminal. It blocks until interrupted.
//
// Nothing is seeded. It clears $SHELL and $PATH before the command reads them,
// so the fallback a run with no origin takes finds no $SHELL, no bash and no
// sh, and ends at the built-in shell: Elvish (https://elv.sh), tunneld's own
// executable run again with a private argument and served on a pseudo-terminal
// like any program. It has a line editor — history, completion, the arrow keys
// — and a language of its own, which is not POSIX.
//
// With $PATH gone, the machine's own programs are gone too, as they would be on
// the machine it stands for; what runs is what the shell brings — ls, cat, cp
// and the rest of the core commands, built into tunneld. Any origin passed
// still replaces the fallback, and every tunneld flag still works:
//
//	go run ./examples/noshell --no-cache
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
	// The context is the tunnel's shutdown handle and the shell's: Ctrl-C
	// tears both down, whether it arrives during startup or after the URL is
	// live.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// A machine with no shell: nothing names one, and there is nowhere to
	// look for bash or sh.
	_ = os.Unsetenv("SHELL")
	_ = os.Unsetenv("PATH")

	cmd := v1alpha1.New(
		v1alpha1.WithLogLevel("debug"),
	).Command()

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, "noshell: "+err.Error())
		os.Exit(1)
	}
}
