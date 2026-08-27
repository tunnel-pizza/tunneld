package docker

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/client"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// discard is the logger every test here uses: these paths log warnings that
// are not what is under test.
func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// TestOpenWithoutDaemon pins the failure an operator hits most: Docker is not
// running. It needs no daemon of its own — pointing DOCKER_HOST at a port
// nothing listens on reproduces exactly the connection failure a stopped
// daemon produces.
func TestOpenWithoutDaemon(t *testing.T) {
	t.Setenv("DOCKER_HOST", "tcp://127.0.0.1:1")
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()

	got, err := Open(ctx, "api", discard())
	if err == nil {
		_ = got.Close()
		t.Fatal("Open with no daemon succeeded, want an error")
	}
	if !errors.Is(err, v1.ErrNoDocker) {
		t.Errorf("error = %v, want %v", err, v1.ErrNoDocker)
	}
}

// withDaemon skips unless a Docker daemon is reachable and the alpine image is
// already local. Pulling would make this lane depend on the network and on a
// registry, which is exactly what the rest of the suite avoids.
func withDaemon(t *testing.T) *client.Client {
	t.Helper()
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		t.Skipf("no docker client: %v", err)
	}
	if _, err := cli.Ping(t.Context()); err != nil {
		_ = cli.Close()
		t.Skipf("no docker daemon: %v", err)
	}
	if _, err := cli.ImageInspect(t.Context(), "alpine"); err != nil {
		_ = cli.Close()
		t.Skip("alpine image not present locally; run: docker pull alpine")
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// startContainer runs an alpine shell and removes it with the test. tty and
// stdin mirror docker run's -t and -i, which is what decides everything the
// attach can do.
func startContainer(t *testing.T, cli *client.Client, tty, stdin bool) string {
	t.Helper()
	created, err := cli.ContainerCreate(t.Context(),
		&container.Config{
			Image:     "alpine",
			Cmd:       []string{"sh"},
			Tty:       tty,
			OpenStdin: stdin,
		}, nil, nil, nil, "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	t.Cleanup(func() {
		_ = cli.ContainerRemove(context.WithoutCancel(t.Context()), created.ID,
			container.RemoveOptions{Force: true})
	})
	if err := cli.ContainerStart(t.Context(), created.ID, container.StartOptions{}); err != nil {
		t.Fatalf("start: %v", err)
	}
	return created.ID
}

// TestOpenRejects pins the two failures that must happen before the tunnel is
// minted: a container that does not exist, and one that is not running. Both
// are the operator's typo or stale assumption, and a public hostname that
// answers only errors is a worse way to learn it.
func TestOpenRejects(t *testing.T) {
	t.Run("no such container", func(t *testing.T) {
		withDaemon(t)
		got, err := Open(t.Context(), "tunneld-test-nonexistent", discard())
		if err == nil {
			_ = got.Close()
			t.Fatal("Open succeeded, want an error")
		}
		if !errors.Is(err, v1.ErrInvalidOrigin) {
			t.Errorf("error = %v, want %v", err, v1.ErrInvalidOrigin)
		}
		if !strings.Contains(err.Error(), "tunneld-test-nonexistent") {
			t.Errorf("error %q does not name the container", err)
		}
	})

	t.Run("container not running", func(t *testing.T) {
		cli := withDaemon(t)
		id := startContainer(t, cli, true, true)
		if err := cli.ContainerStop(t.Context(), id, container.StopOptions{}); err != nil {
			t.Fatalf("stop: %v", err)
		}
		got, err := Open(t.Context(), id, discard())
		if err == nil {
			_ = got.Close()
			t.Fatal("Open on a stopped container succeeded, want an error")
		}
		if !errors.Is(err, v1.ErrInvalidOrigin) {
			t.Errorf("error = %v, want %v", err, v1.ErrInvalidOrigin)
		}
	})

	// Config is a pointer in the API's own model, and Open reads two fields
	// off it. Real Docker always sends one, which is exactly why this case
	// needs a stub daemon rather than a container: the shapes that would
	// arrive without it — an old API version, something answering the socket
	// that is not Docker — cannot be produced locally, and the alternative to
	// pinning it here is finding out through a nil dereference in production.
	t.Run("no container configuration", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Api-Version", "1.51")
			w.Header().Set("Ostype", "linux")
			if strings.HasSuffix(r.URL.Path, "/_ping") {
				return
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"Id":"deadbeef","State":{"Running":true}}`)
		}))
		t.Cleanup(srv.Close)
		t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))

		got, err := Open(t.Context(), "shim", discard())
		if err == nil {
			_ = got.Close()
			t.Fatal("Open on a container with no config succeeded, want an error")
		}
		if !errors.Is(err, v1.ErrInvalidOrigin) {
			t.Errorf("error = %v, want %v", err, v1.ErrInvalidOrigin)
		}
		if !strings.Contains(err.Error(), "shim") {
			t.Errorf("error %q does not name the container", err)
		}
	})
}

// TestAttach pins the round trip against a real container, both ways it can be
// started. The TTY case copies straight through; the non-TTY case has to be
// demultiplexed, and getting that backwards produces output with 8-byte
// headers embedded in it rather than an error.
func TestAttach(t *testing.T) {
	cli := withDaemon(t)

	cases := []struct {
		name  string
		tty   bool
		stdin bool
	}{
		{"started with -it", true, true},
		{"started with -i, no tty", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id := startContainer(t, cli, tc.tty, tc.stdin)
			a, err := Open(t.Context(), id, discard())
			if err != nil {
				t.Fatalf("Open: %v", err)
			}
			defer a.Close()

			if a.TTY() != tc.tty {
				t.Errorf("TTY() = %v, want %v", a.TTY(), tc.tty)
			}
			if a.Stdin() != tc.stdin {
				t.Errorf("Stdin() = %v, want %v", a.Stdin(), tc.stdin)
			}

			ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
			defer cancel()

			stdinR, stdinW := io.Pipe()
			out := &syncBuffer{}
			resize := make(chan remotecommand.TerminalSize)
			done := make(chan error, 1)
			go func() {
				done <- a.AttachContainer(ctx, id, "", id, stdinR, nopCloser{out}, nopCloser{out}, tc.tty, resize)
			}()

			if _, err := io.WriteString(stdinW, "echo tunneld-marker\n"); err != nil {
				t.Fatalf("write stdin: %v", err)
			}

			deadline := time.After(20 * time.Second)
			for !strings.Contains(out.String(), "tunneld-marker") {
				select {
				case <-deadline:
					t.Fatalf("never saw the marker; got %q", out.String())
				case err := <-done:
					t.Fatalf("attach ended early: %v (output %q)", err, out.String())
				case <-time.After(200 * time.Millisecond):
				}
			}

			// A stdcopy frame precedes its payload with 8 bytes: a stream byte
			// (0, 1 or 2), three reserved zero bytes, then a 4-byte big-endian
			// length. "\x01\x00\x00\x00" is the stream byte and reserve of a
			// stdout frame — not a sequence an alpine shell has any reason to
			// print — so finding it in the output means AttachContainer forwarded
			// stdcopy's own framing instead of stripping it. That is a bug the
			// marker check above cannot see: the header lands as a clean prefix
			// ahead of the payload rather than inside it, so the marker still
			// turns up whether or not the demux ran. This holds for both
			// subtests, not just the non-TTY one: a TTY container is never
			// stdcopy-framed by the daemon in the first place, so the header is
			// equally absent whichever path handled it.
			if strings.Contains(out.String(), "\x01\x00\x00\x00") {
				t.Errorf("output carries a stdcopy frame header, demux did not run: %q", out.String())
			}

			// A cancelled context is the only shutdown signal a Target gets —
			// the tunnel going down and the visitor leaving both arrive here
			// as exactly this and nothing else. Note what is deliberately not
			// done first: stdin is left open, so nothing on that side can end
			// the attach on the implementation's behalf. sh has printed its
			// prompt and is waiting for input it will never get, so the output
			// copy is parked in Read on a socket that will never produce
			// another byte — which is precisely the state an abandoned session
			// is in. Without something closing that socket on cancel, this
			// never returns, and every abandoned session leaks a goroutine and
			// a daemon connection for the life of the process. Asserted on a
			// deadline rather than a bare receive so a regression fails in
			// five seconds instead of hanging the lane.
			cancel()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("AttachContainer did not return within 5s of its context being cancelled")
			}
			_ = stdinW.Close()
		})
	}
}

// syncBuffer is a bytes.Buffer the attach goroutine writes while the test
// reads. Without the mutex the race detector fails the whole lane.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// nopCloser adapts a writer to the io.WriteCloser the Attacher contract takes.
type nopCloser struct{ io.Writer }

func (nopCloser) Close() error { return nil }
