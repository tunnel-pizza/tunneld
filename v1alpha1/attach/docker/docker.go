// Package docker attaches to a running container over the Docker Engine API,
// implementing the Target the attach package serves.
//
// It knows nothing about HTTP or websockets: it is handed the four streams and
// a resize channel, and it copies. The split is what keeps a second provider —
// podman, or a local shell over a pty — from having to touch the server.
package docker

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"github.com/docker/docker/pkg/stdcopy"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// Attacher is one container, resolved and inspected.
type Attacher struct {
	cli   *client.Client
	log   *slog.Logger
	id    string
	ref   string
	tty   bool
	stdin bool
}

// Open resolves ref — a container name or id — against the daemon named by the
// environment ($DOCKER_HOST and friends) and inspects it.
//
// Everything that can fail about a container origin fails here, before the
// tunnel is minted. A public hostname that answers only errors is a far worse
// way to learn that a name is misspelled, and by the time the tunnel is up the
// operator has already been handed a URL to share.
//
// The three failures are told apart because their levers differ: a daemon that
// cannot be reached is ErrNoDocker (start Docker), and both a missing container
// and a stopped one are ErrInvalidOrigin (fix the --url, or start it).
func Open(ctx context.Context, ref string, log *slog.Logger) (*Attacher, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("%w: %w", v1.ErrNoDocker, err)
	}

	info, err := cli.ContainerInspect(ctx, ref)
	if err != nil {
		_ = cli.Close()
		switch {
		case client.IsErrNotFound(err):
			return nil, fmt.Errorf("%w: no container named %q", v1.ErrInvalidOrigin, ref)
		case client.IsErrConnectionFailed(err):
			return nil, fmt.Errorf("%w: %w", v1.ErrNoDocker, err)
		default:
			// Anything else — a permission denial on the socket, an API
			// version the daemon refuses — is still the daemon's answer, not
			// this container's, so it reads as a daemon problem.
			return nil, fmt.Errorf("%w: inspecting %q: %w", v1.ErrNoDocker, ref, err)
		}
	}
	if info.State == nil || !info.State.Running {
		_ = cli.Close()
		return nil, fmt.Errorf("%w: container %q is not running", v1.ErrInvalidOrigin, ref)
	}
	// Config is what says whether the container was started with -t and -i,
	// and both fields are read unconditionally below. The API models it as a
	// pointer, so a daemon that answers without one — an old API version, a
	// shim that is not Docker — would panic here rather than fail. It reads as
	// an origin problem for the same reason a stopped container does: nothing
	// tunneld can do makes this container attachable, and the lever is the
	// --url.
	if info.Config == nil {
		_ = cli.Close()
		return nil, fmt.Errorf("%w: container %q reports no configuration", v1.ErrInvalidOrigin, ref)
	}

	return &Attacher{
		cli:   cli,
		log:   log,
		id:    info.ID,
		ref:   ref,
		tty:   info.Config.Tty,
		stdin: info.Config.OpenStdin,
	}, nil
}

// Name is the reference the operator typed, not the resolved id: it is what
// they will recognize in a page title and a log line.
func (a *Attacher) Name() string { return a.ref }

// TTY reports Config.Tty — whether the container was started with -t. It is
// fixed at docker run time and nothing here can change it.
func (a *Attacher) TTY() bool { return a.tty }

// Stdin reports Config.OpenStdin — whether the container was started with -i.
func (a *Attacher) Stdin() bool { return a.stdin }

// Close releases the API client.
func (a *Attacher) Close() error { return a.cli.Close() }

// AttachContainer attaches to PID 1 and copies until the stream ends or ctx is
// canceled, which is what `docker attach` does. The Kubernetes-shaped name,
// uid and container arguments are ignored: this Attacher is one container by
// construction.
//
// Logs is on, which is the one place this departs from `docker attach`: the
// backlog replays so that opening the URL shows what the container has been
// printing. An empty terminal on a quiet container is indistinguishable from a
// broken one, and this page is usually opened long after the container started.
func (a *Attacher) AttachContainer(ctx context.Context, _, _, _ string, in io.Reader, out, errw io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	resp, err := a.cli.ContainerAttach(ctx, a.id, container.AttachOptions{
		Stream: true,
		Stdin:  a.stdin,
		Stdout: true,
		Stderr: true,
		Logs:   true,
	})
	if err != nil {
		return fmt.Errorf("attach %s: %w", a.ref, err)
	}
	defer resp.Close()

	// ctx got the connection dialed and then stopped mattering: once the
	// daemon hands the socket over, resp.Reader is a bare net.Conn with
	// nothing left tying it to a context. Neither shutdown path reaches a copy
	// parked in Read on a quiet container either — net/http.Server.Close
	// deliberately leaves hijacked connections alone, and client.Close only
	// drops *idle* transport connections, which this is not. Closing the
	// socket is the one thing that unblocks that Read, so it is what makes the
	// "or ctx is canceled" half of this function's promise true rather than
	// aspirational; the error it produces is already read as a normal end
	// below. AfterFunc rather than a goroutine parked on ctx.Done(), because
	// stop() on the way out is what keeps the watcher from outliving the
	// attach on a context that is never canceled at all.
	stop := context.AfterFunc(ctx, func() { resp.Close() })
	defer stop()

	go a.watchResize(ctx, resize)

	if a.stdin && in != nil {
		go func() {
			// Bounded from both ends: a write parked on resp.Conn dies with
			// the socket the watcher above closes, and a read parked on in
			// ends when the caller closes it — which ServeAttach does the
			// moment this function returns, since tearing the websocket down
			// closes every channel on it.
			_, _ = io.Copy(resp.Conn, in)
			// Half-close, so a container reading to EOF sees one. Closing the
			// whole connection here would cut its output off mid-sentence.
			_ = resp.CloseWrite()
		}()
	}

	// With a TTY the container merged the two streams itself and the bytes are
	// raw. Without one they arrive stdcopy-framed — an 8-byte header per chunk
	// carrying stream id and length — and demultiplexing is what puts the
	// container's stderr on a channel of its own instead of printing the
	// headers into somebody's terminal.
	if tty {
		_, err = io.Copy(out, resp.Reader)
	} else {
		_, err = stdcopy.StdCopy(out, errw, resp.Reader)
	}

	// PID 1 exiting, the visitor leaving, and the tunnel shutting down all
	// arrive here as a closed pipe. None of them is a failure worth reporting:
	// the stream ending is the normal end of an attach.
	if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, context.Canceled) {
		err = nil
	}
	return err
}

// watchResize forwards terminal sizes to the container, and returns when the
// channel closes — which ServeAttach does when the socket ends.
//
// Without a TTY there is nothing to resize, but the channel is still drained:
// the page sends its size as a heartbeat regardless, and a blocked send would
// stall the whole stream.
func (a *Attacher) watchResize(ctx context.Context, resize <-chan remotecommand.TerminalSize) {
	for size := range resize {
		if !a.tty || size.Width == 0 || size.Height == 0 {
			continue
		}
		err := a.cli.ContainerResize(ctx, a.id, container.ResizeOptions{
			Height: uint(size.Height),
			Width:  uint(size.Width),
		})
		if err != nil && ctx.Err() == nil {
			a.log.Debug("could not resize container", "container", a.ref, "error", err)
		}
	}
}
