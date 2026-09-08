// Command attach exposes a container's terminal through a tunnel.
//
// It starts the container too — nothing else needs to be running — and puts a
// shell in it on a public hostname:
//
//	https://<host>/   -> dockerd://tunneld-example
//
// A dockerd:// origin is not proxied like the others. A container is not an
// HTTP service, so tunneld becomes one on its behalf: it serves a terminal
// page and streams the container's stdio to it, and the origin behaves like
// any other from there — it takes an index, it gets a multiview tile, and it
// mixes freely with http:// origins.
//
// The container is started with a TTY and stdin open, which is what makes the
// terminal interactive. Started without them the page still works and says so,
// but there is nothing to type into. Ctrl-C in the shell stops the container,
// exactly as `docker attach` would: with a TTY the signal reaches PID 1, and
// PID 1 is the shell.
//
// Needs a Docker daemon. The image is pulled if it is not already local.
package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/client"
	"github.com/spf13/cobra"
	"github.com/tunnel-pizza/tunneld/v1alpha1"
)

// name is the container this example attaches to. Fixed rather than generated,
// because the origin has to be seeded before the command is built and a
// generated id would not be known that early.
const name = "tunneld-example"

// image is the container to attach to: a zsh that runs as PID 1, so Ctrl-C
// reaches it and the terminal behaves the way a shell on any other machine
// does.
const image = "ghcr.io/cnuss/zsh:latest"

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)

	cmd := v1alpha1.New(
		v1alpha1.WithOrigin("dockerd://"+name),
		v1alpha1.WithLogLevel("debug"),
	).Command()

	// PreRunE rather than plain code before ExecuteContext: cobra answers
	// --help before that hook runs, so `--help` stays a pure question and
	// touches no daemon.
	var remove func()
	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		// Bring up the container this example exposes, returning once it is
		// running, so the tunnel never attaches to something that is not there
		// yet, along with the function that takes it down again.
		//
		// The teardown is returned rather than hung on ctx because it has to survive
		// ctx: by the time the caller wants it, the context that started the container
		// is usually the one that just ended.
		ctx := cmd.Context()

		cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
		if err != nil {
			return fmt.Errorf("docker client: %w", err)
		}

		// Pull the image if it is not already local. Inspect first, because the
		// common case is that it is, and a pull that only prints "up to date" is
		// still a round trip to a registry the example does not need.
		if _, err := cli.ImageInspect(ctx, image); err != nil {
			if !cerrdefs.IsNotFound(err) {
				_ = cli.Close()
				return fmt.Errorf("inspect %s: %w", image, err)
			}

			fmt.Fprintln(os.Stderr, "attach: pulling "+image)
			body, err := cli.ImagePull(ctx, image, client.ImagePullOptions{})
			if err != nil {
				_ = cli.Close()
				return fmt.Errorf("pull %s: %w", image, err)
			}
			defer body.Close()
			// The pull happens as the body is read; discarding it is what waits for
			// the layers, and stopping early would leave the image half-fetched.
			if _, err := io.Copy(io.Discard, body); err != nil && !errors.Is(err, context.Canceled) {
				_ = cli.Close()
				return fmt.Errorf("pull %s: %w", image, err)
			}
		}

		// A leftover from a previous run is reused when it is still running and
		// replaced when it is not. Reusing rather than recreating means an
		// interrupted run does not throw away a shell somebody was using; replacing
		// a stopped one means this never attaches to a corpse.
		switch res, err := cli.ContainerInspect(ctx, name, client.ContainerInspectOptions{}); {
		case err == nil && res.Container.State != nil && res.Container.State.Running:
			// Somebody may be typing in it; leave it, and leave it behind too.
			remove = func() { _ = cli.Close() }
			return nil
		case err == nil:
			if _, err := cli.ContainerRemove(ctx, res.Container.ID, client.ContainerRemoveOptions{Force: true}); err != nil {
				_ = cli.Close()
				return fmt.Errorf("remove the stopped %s: %w", name, err)
			}
		case !cerrdefs.IsNotFound(err):
			_ = cli.Close()
			return fmt.Errorf("inspect %s: %w", name, err)
		}

		// Tty and OpenStdin are docker run's -t and -i. They are what make the
		// attached terminal interactive, and nothing can add them afterwards.
		created, err := cli.ContainerCreate(ctx, client.ContainerCreateOptions{
			Config: &container.Config{
				Image: image,
				// No Cmd. The image's entrypoint is zsh itself, so anything
				// here arrives as an argument to it rather than as the command
				// to run — a "sh" would have it looking for a script by that
				// name. The shell the image ships with is the one to attach to.
				Tty:       true,
				OpenStdin: true,
			},
			HostConfig: &container.HostConfig{AutoRemove: true},
			Name:       name,
		})
		if err != nil {
			_ = cli.Close()
			return fmt.Errorf("create %s: %w", name, err)
		}
		if _, err := cli.ContainerStart(ctx, created.ID, client.ContainerStartOptions{}); err != nil {
			_ = cli.Close()
			return fmt.Errorf("start %s: %w", name, err)
		}

		// AutoRemove covers the container stopping on its own; this covers the
		// tunnel ending, by a signal or by failing, which would otherwise leave it
		// running. WithoutCancel because the context is usually already done.
		remove = func() {
			_, _ = cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, client.ContainerRemoveOptions{Force: true})
			_ = cli.Close()
		}
		return nil
	}

	// os.Exit runs no deferred function, so remove and stop are called
	// explicitly here rather than deferred: exiting before them would strand
	// the container — the tunnel failing is an ordinary outcome, an
	// unreachable edge or a revoked hostname, and it must still take the
	// container with it. remove goes first: stop releases the signal
	// handler, so a second Ctrl-C during docker rm -f would kill the process
	// before the container is gone.
	err := cmd.ExecuteContext(ctx)
	if remove != nil {
		remove()
	}
	stop()
	if err != nil {
		fmt.Fprintln(os.Stderr, "attach: "+err.Error())
		os.Exit(1)
	}
}
