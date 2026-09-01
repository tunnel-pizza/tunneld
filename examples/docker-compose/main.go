package main

import (
	"context"
	_ "embed"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"strings"
	"syscall"
	"time"
)

func main() {
	// os.Exit runs no deferred function, so the whole program lives in run and
	// main does nothing but report. Exiting from inside run would skip the
	// `compose down` and leave the whole stack standing.
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	// Ctrl-C is the way out, not a timeout: this brings up a stack and hands
	// it to you, and there is no point at which it has been up long enough.
	// The signal cancels ctx, which interrupts `compose up`.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	setup()

	// Armed before `up` blocks, so an interrupt or a failure still takes the
	// stack down — registering it afterwards would mean nothing was armed for
	// the entire time the stack was actually up.
	//
	// WithoutCancel because ctx is normally already cancelled by the time this
	// runs: that cancellation is what ended `up`, and inheriting it would kill
	// `down` before it started.
	defer func() {
		if err := sh(context.WithoutCancel(ctx), "docker compose down --volumes --remove-orphans"); err != nil {
			log.Print(err)
		}
	}()

	return sh(ctx, "docker compose up")
}

// sh runs one command with this process's stdio, the way a line of a shell
// script would.
//
// Cancelling ctx interrupts the command rather than killing it: `docker
// compose up` stops its own containers on SIGINT, while the default SIGKILL
// would leave them running for the `down` above to find. WaitDelay bounds how
// long it may take about it before the kill happens anyway.
//
// A command that ends because ctx was cancelled is not a failure — that is the
// interrupt doing exactly what it was asked to.
func sh(ctx context.Context, command string) error {
	args := strings.Split(command, " ")
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second

	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		return fmt.Errorf("%s: %w", command, err)
	}
	return nil
}

func setup() {
	if err := os.WriteFile(path.Join(workDir, "docker-compose.yml"), dockerComposeYml, 0644); err != nil {
		log.Fatalf("failed to write docker-compose.yml: %v", err)
	}
	if err := os.Chdir(workDir); err != nil {
		log.Fatalf("failed to change directory to %s: %v", workDir, err)
	}
}

var (
	//go:embed docker-compose.yml
	dockerComposeYml []byte
	workDir          = func() string {
		tmpDir, err := os.MkdirTemp("", "tunneld-docker-compose-*")
		if err != nil {
			log.Fatalf("failed to create temporary directory: %v", err)
		}
		return tmpDir
	}()
)
