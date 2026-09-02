// Package docker attaches to a running container over the Docker Engine API,
// implementing the Target the attach package serves.
//
// It knows nothing about HTTP or websockets: it is handed the four streams and
// a resize channel, and it copies. The split is what keeps a second provider —
// podman, or a local shell over a pty — from having to touch the server.
package docker

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"regexp"
	"slices"
	"strings"

	cerrdefs "github.com/containerd/errdefs"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
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

// Open resolves ref — a container name, an id, or a Compose service — against
// the daemon named by the environment ($DOCKER_HOST and friends) and inspects
// it.
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
	// Under Compose the name a person knows is not the name the daemon knows:
	// service `web` in project `proj` is a container called `proj-web-1`, so
	// the obvious TUNNELD_URL misses every time. Falling back to the labels
	// Compose already writes costs one list call, and only on the path that
	// has failed anyway. Name-or-id stays first: a container literally named
	// `web` must keep winning, or this changes what an existing config means.
	if cerrdefs.IsNotFound(err) {
		if id, serr := resolveService(ctx, cli, ref); serr != nil {
			err = serr
		} else if id != "" {
			info, err = cli.ContainerInspect(ctx, id)
		}
	}
	if err != nil {
		_ = cli.Close()
		switch {
		// An ambiguous service already names its candidates and the lever.
		case errors.Is(err, v1.ErrInvalidOrigin):
			return nil, err
		case cerrdefs.IsNotFound(err):
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

// The labels Compose writes on every container it starts. They are the whole
// mechanism here: nothing has to shell out to the compose plugin or parse a
// compose file.
const (
	composeProject = "com.docker.compose.project"
	composeService = "com.docker.compose.service"
)

// resolveService looks ref up as a Compose service name. It returns the id of
// the single container that matches, "" when nothing does — leaving the
// caller's original "no such container" error to stand — or an error when the
// name is ambiguous.
//
// Ambiguity is an error rather than a pick, because the alternative is an
// origin that quietly points at a different replica after a restart.
//
// All is set so a stopped service is found: it then fails the running check
// with "container %q is not running", which names the lever, instead of
// degrading to "no container named" for a container that plainly exists.
func resolveService(ctx context.Context, cli *client.Client, ref string) (string, error) {
	f := filters.NewArgs(filters.Arg("label", composeService+"="+ref))
	// Scoping to tunneld's own project is what keeps `web` from being
	// ambiguous on a machine busy enough to run two of them — which is
	// precisely when it matters. Off the scope, an ambiguous match is still an
	// error rather than a coin flip, so the unscoped case stays safe.
	if project := ownProject(ctx, cli); project != "" {
		f.Add("label", composeProject+"="+project)
	}

	found, err := cli.ContainerList(ctx, container.ListOptions{All: true, Filters: f})
	if err != nil || len(found) == 0 {
		return "", nil
	}
	if len(found) == 1 {
		return found[0].ID, nil
	}

	names := make([]string, 0, len(found))
	for _, c := range found {
		name := c.ID
		if len(c.Names) > 0 {
			name = strings.TrimPrefix(c.Names[0], "/")
		}
		if p := c.Labels[composeProject]; p != "" {
			name += " (project " + p + ")"
		}
		names = append(names, name)
	}
	slices.Sort(names)
	return "", fmt.Errorf("%w: service %q matches %d containers: %s — name one of them instead",
		v1.ErrInvalidOrigin, ref, len(found), strings.Join(names, ", "))
}

// mountinfo is where selfRefs looks for this process's own container id. A
// variable because the platforms this package is tested on mostly have no
// /proc at all, and the parsing is worth pinning anyway.
var mountinfo = "/proc/self/mountinfo"

// containerID matches a 64-hex path component. Both container directories and
// layer directories are named that way, which is why selfIDs orders its
// answers rather than picking one.
var containerID = regexp.MustCompile(`[0-9a-f]{64}`)

// selfRefsMax bounds how many candidate ids are tried. Each one costs an
// inspect, and a mountinfo that offers a dozen 64-hex paths is one this is not
// reading correctly — better to widen the lookup than to interrogate the
// daemon about layer directories.
const selfRefsMax = 4

// ownProject names the Compose project tunneld itself belongs to, or "" if it
// does not belong to one.
//
// The references are guesses, checked by being inspected: one that names no
// container answers 404 and the next is tried. Nothing found means "", which
// widens the lookup to the whole host — the wrong project would be a far worse
// answer than no project, so every uncertain path ends here.
func ownProject(ctx context.Context, cli *client.Client) string {
	for _, ref := range selfRefs() {
		info, err := cli.ContainerInspect(ctx, ref)
		if err != nil || info.Config == nil {
			continue
		}
		if project := info.Config.Labels[composeProject]; project != "" {
			return project
		}
	}
	return ""
}

// selfRefs returns what might name this process's own container, cheapest and
// most portable first.
//
// The hostname is the normal case: Compose sets it to the container id. But a
// service that sets `hostname:` makes it name nothing the daemon knows — and
// that is not an exotic option, it is what `network_mode: service:<name>`
// needs, which is the shape this scoping exists to serve. The id is still in
// mountinfo, in the paths the runtime bind-mounts in, and nothing else inside
// the container carries it: cgroup reads `0::/` under v2 in a namespace.
func selfRefs() []string {
	var refs []string
	if host, err := os.Hostname(); err == nil && host != "" {
		refs = append(refs, host)
	}

	f, err := os.Open(mountinfo)
	if err != nil {
		return refs // not Linux, or not in a container
	}
	defer func() { _ = f.Close() }()
	return append(refs, selfIDs(f)...)
}

// selfIDs pulls candidate container ids out of mountinfo, most likely first.
//
// An id under a .../containers/<id>/... path is the runtime's own
// per-container directory — the one that bind-mounts /etc/hosts and
// /etc/hostname — so it leads. Anything else is most likely a layer directory
// that merely looks the same, kept only as a fallback because the layout of
// this file is the runtime's business rather than an interface it owes anyone.
func selfIDs(r io.Reader) []string {
	preferred := map[string]bool{}
	seen := map[string]bool{}
	var order []string

	scan := bufio.NewScanner(r)
	for scan.Scan() {
		line := scan.Text()
		for _, id := range containerID.FindAllString(line, -1) {
			if !seen[id] {
				seen[id] = true
				order = append(order, id)
			}
			if strings.Contains(line, "/containers/"+id) {
				preferred[id] = true
			}
		}
	}

	ids := make([]string, 0, len(order))
	for _, want := range []bool{true, false} {
		for _, id := range order {
			if preferred[id] == want && len(ids) < selfRefsMax {
				ids = append(ids, id)
			}
		}
	}
	return ids
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
