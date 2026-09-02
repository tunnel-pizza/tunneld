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
// Needs a Docker daemon. The alpine image is pulled if it is not already
// local.
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
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/image"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
	"github.com/tunnel-pizza/tunneld/v1alpha1"
)

// name is the container this example attaches to. Fixed rather than generated,
// because the origin has to be seeded before the command is built and a
// generated id would not be known that early.
const name = "tunneld-example"

func main() {
	// os.Exit runs no deferred function, so the whole program lives in run and
	// main does nothing but report. Exiting from inside run would strand the
	// container: the tunnel failing is an ordinary outcome — an unreachable
	// edge, a revoked hostname — and it must still take the container with it.
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "attach: "+err.Error())
		os.Exit(1)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd := v1alpha1.New().
		WithURL("dockerd://" + name).
		Build()

	// PreRunE rather than plain code before ExecuteContext: cobra answers
	// --help before that hook runs, so `--help` stays a pure question and
	// touches no daemon.
	var remove func()
	cmd.PreRunE = func(cmd *cobra.Command, _ []string) error {
		var err error
		remove, err = start(cmd.Context())
		return err
	}
	defer func() {
		if remove != nil {
			remove()
		}
	}()

	return cmd.ExecuteContext(ctx)
}

// start brings up the container this example exposes and returns once it is
// running, so the tunnel never attaches to something that is not there yet,
// along with the function that takes it down again.
//
// The teardown is returned rather than hung on ctx because it has to survive
// ctx: by the time the caller wants it, the context that started the container
// is usually the one that just ended.
func start(ctx context.Context) (func(), error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("docker client: %w", err)
	}

	if err := ensureImage(ctx, cli); err != nil {
		_ = cli.Close()
		return nil, err
	}

	// A leftover from a previous run is reused when it is still running and
	// replaced when it is not. Reusing rather than recreating means an
	// interrupted run does not throw away a shell somebody was using; replacing
	// a stopped one means this never attaches to a corpse.
	switch info, err := cli.ContainerInspect(ctx, name); {
	case err == nil && info.State != nil && info.State.Running:
		// Somebody may be typing in it; leave it, and leave it behind too.
		return func() { _ = cli.Close() }, nil
	case err == nil:
		if err := cli.ContainerRemove(ctx, info.ID, container.RemoveOptions{Force: true}); err != nil {
			_ = cli.Close()
			return nil, fmt.Errorf("remove the stopped %s: %w", name, err)
		}
	case !cerrdefs.IsNotFound(err):
		_ = cli.Close()
		return nil, fmt.Errorf("inspect %s: %w", name, err)
	}

	// Tty and OpenStdin are docker run's -t and -i. They are what make the
	// attached terminal interactive, and nothing can add them afterwards.
	created, err := cli.ContainerCreate(ctx,
		&container.Config{
			Image:     "alpine",
			Cmd:       []string{"sh"},
			Tty:       true,
			OpenStdin: true,
		},
		&container.HostConfig{AutoRemove: true}, nil, nil, name)
	if err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("create %s: %w", name, err)
	}
	if err := cli.ContainerStart(ctx, created.ID, container.StartOptions{}); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("start %s: %w", name, err)
	}

	// AutoRemove covers the container stopping on its own; this covers the
	// tunnel ending, by a signal or by failing, which would otherwise leave it
	// running. WithoutCancel because the context is usually already done.
	return func() {
		_ = cli.ContainerRemove(context.WithoutCancel(ctx), created.ID, container.RemoveOptions{Force: true})
		_ = cli.Close()
	}, nil
}

// ensureImage pulls alpine if it is not already local. Inspect first, because
// the common case is that it is, and a pull that only prints "up to date" is
// still a round trip to a registry the example does not need.
func ensureImage(ctx context.Context, cli *client.Client) error {
	if _, err := cli.ImageInspect(ctx, "alpine"); err == nil {
		return nil
	} else if !cerrdefs.IsNotFound(err) {
		return fmt.Errorf("inspect the alpine image: %w", err)
	}

	fmt.Fprintln(os.Stderr, "attach: pulling alpine")
	body, err := cli.ImagePull(ctx, "alpine", image.PullOptions{})
	if err != nil {
		return fmt.Errorf("pull alpine: %w", err)
	}
	defer body.Close()
	// The pull happens as the body is read; discarding it is what waits for
	// the layers, and stopping early would leave the image half-fetched.
	if _, err := io.Copy(io.Discard, body); err != nil && !errors.Is(err, context.Canceled) {
		return fmt.Errorf("pull alpine: %w", err)
	}
	return nil
}
