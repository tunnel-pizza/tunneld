package main

import (
	"context"
	_ "embed"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"
)

//go:embed docker-compose.yml
var dockerComposeYml []byte

func main() {
	// Deferred first so it runs last: os.Exit runs no defer, and the stack has
	// to come down before this process does.
	code := 0
	defer func() { os.Exit(code) }()

	// Ctrl-C or thirty seconds, whichever lands first. Either one cancels ctx,
	// which interrupts `up` below and lets the teardown run — an example
	// should not hold a stack open on someone's machine indefinitely.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	dir := func() string {
		tunneld, _ := os.Getwd()
		// slice 2 levels from cwd
		tunneld = tunneld[:strings.LastIndex(tunneld, "/")]
		tunneld = tunneld[:strings.LastIndex(tunneld, "/")]

		dir, err := os.MkdirTemp("", "tunneld-example-")
		if err != nil {
			log.Fatalf("temporary directory: %v", err)
		}

		if err := os.WriteFile(dir+"/docker-compose.yml", dockerComposeYml, 0o600); err != nil {
			_ = os.RemoveAll(dir)
			log.Fatalf("write the compose file: %v", err)
		}

		// symlink tunneld to the temporary directory so that the docker-compose.yml can find it
		err = os.Symlink(tunneld, dir+"/tunneld")
		if err != nil {
			_ = os.RemoveAll(dir)
			log.Fatalf("symlink tunneld: %v", err)
		}

		return dir
	}()

	// Deferred before `up` blocks, so an interrupt still takes the stack down.
	// Background, not ctx: by then ctx is cancelled — that is what ended `up` —
	// and inheriting it would kill `down` before it started.
	teardown := func() {
		sh(context.Background(), dir,
			"docker compose -f docker-compose.yml -p tunneld-example down --remove-orphans")
		_ = os.RemoveAll(dir)
	}
	defer teardown()

	// -p is named rather than derived from the working directory, which is a
	// fresh temporary one every run: a name that changes strands the stack of
	// any run that is interrupted. The teardown spells the same one.
	if err := sh(ctx, dir, "docker compose -f docker-compose.yml -p tunneld-example up --pull always"); err != nil {
		log.Printf("compose up: %v", err)
		code = 1
	}
}

// sh runs one line of a shell script in dir, on this process's stdio.
func sh(ctx context.Context, dir, command string) error {
	args := strings.Fields(command)
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = dir
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	// SIGINT, not the default SIGKILL: compose stops its own containers on an
	// interrupt, where a kill would leave them for `down` to find.
	cmd.Cancel = func() error { return cmd.Process.Signal(os.Interrupt) }
	cmd.WaitDelay = 10 * time.Second

	// A command that ended because ctx was cancelled did what it was asked.
	if err := cmd.Run(); err != nil && ctx.Err() == nil {
		return err
	}
	return nil
}
