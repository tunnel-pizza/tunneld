package docker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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

// composeContainer is one container the stub daemon in composeDaemon knows
// about, described the way Compose labels a real one.
type composeContainer struct {
	id, name, project, service string
}

// composeDaemon stands up a stub Docker API serving the three calls the
// service lookup makes: the inspect that misses, the label-filtered list, and
// the inspect of whatever that list resolved to.
//
// It is a stub rather than a real daemon because the case under test is a
// *Compose* deployment — reproducing one locally means writing a compose file,
// starting a project, and depending on the compose plugin being installed,
// none of which the rest of this suite needs. The labels are the entire
// mechanism, and they are ordinary strings.
//
// selfProject is the com.docker.compose.project label reported for this
// process's own hostname — "" means tunneld is not running inside a project,
// which is the host-side case.
func composeDaemon(t *testing.T, selfProject string, cs ...composeContainer) {
	t.Helper()
	host, err := os.Hostname()
	if err != nil {
		t.Skipf("no hostname: %v", err)
	}

	labels := func(c composeContainer) map[string]string {
		return map[string]string{
			"com.docker.compose.project": c.project,
			"com.docker.compose.service": c.service,
		}
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Api-Version", "1.51")
		w.Header().Set("Ostype", "linux")
		if strings.HasSuffix(r.URL.Path, "/_ping") {
			return
		}
		w.Header().Set("Content-Type", "application/json")

		// The label-filtered list. filters arrives as JSON in the query, and
		// every label the caller asked for has to match for a container to.
		if strings.HasSuffix(r.URL.Path, "/containers/json") {
			var want map[string]map[string]bool
			_ = json.Unmarshal([]byte(r.URL.Query().Get("filters")), &want)
			out := []container.Summary{}
			for _, c := range cs {
				keep := true
				for kv := range want["label"] {
					k, v, _ := strings.Cut(kv, "=")
					if labels(c)[k] != v {
						keep = false
					}
				}
				if keep {
					out = append(out, container.Summary{
						ID:     c.id,
						Names:  []string{"/" + c.name},
						Labels: labels(c),
					})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}

		// Everything else is an inspect. The reference is the path element
		// before /json.
		ref := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/containers/"), "/json")
		if i := strings.LastIndex(ref, "/containers/"); i >= 0 {
			ref = ref[i+len("/containers/"):]
		}

		// This process's own container, which is how the project gets scoped.
		if ref == host && selfProject != "" {
			_, _ = io.WriteString(w, `{"Id":"self","State":{"Running":true},`+
				`"Config":{"Labels":{"com.docker.compose.project":"`+selfProject+`"}}}`)
			return
		}

		for _, c := range cs {
			if ref == c.id || ref == c.name {
				cfg, _ := json.Marshal(map[string]any{
					"Tty": true, "OpenStdin": true, "Labels": labels(c),
				})
				_, _ = io.WriteString(w, `{"Id":"`+c.id+`","State":{"Running":true},`+
					`"Config":`+string(cfg)+`}`)
				return
			}
		}
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"message":"No such container: `+ref+`"}`)
	}))
	t.Cleanup(srv.Close)
	t.Setenv("DOCKER_HOST", "tcp://"+strings.TrimPrefix(srv.URL, "http://"))
}

// TestOpenResolvesComposeService pins the fallback that makes a Compose
// deployment work: under Compose the name a person knows is the service, and
// the name the daemon knows is <project>-<service>-<n>, so an inspect on the
// service alone always misses.
func TestOpenResolvesComposeService(t *testing.T) {
	t.Run("service name resolves to its container", func(t *testing.T) {
		composeDaemon(t, "", composeContainer{
			id: "abc123", name: "containers-claude-code-1",
			project: "containers", service: "claude-code",
		})

		got, err := Open(t.Context(), "claude-code", discard())
		if err != nil {
			t.Fatalf("Open on a Compose service: %v", err)
		}
		defer func() { _ = got.Close() }()
		if got.id != "abc123" {
			t.Errorf("id = %q, want %q", got.id, "abc123")
		}
		// The page title and the log line say what the operator typed, not
		// the generated container name they never chose.
		if got.Name() != "claude-code" {
			t.Errorf("Name() = %q, want %q", got.Name(), "claude-code")
		}
	})

	// A container literally named `web` has to keep winning, or this changes
	// the meaning of a config that already works.
	t.Run("a real container name still wins", func(t *testing.T) {
		composeDaemon(t, "",
			composeContainer{id: "plain", name: "web"},
			composeContainer{id: "svc", name: "proj-web-1", project: "proj", service: "web"},
		)

		got, err := Open(t.Context(), "web", discard())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = got.Close() }()
		if got.id != "plain" {
			t.Errorf("id = %q, want the container named web (%q)", got.id, "plain")
		}
	})

	// Two projects each running a `web` is exactly when the fallback is
	// ambiguous, and exactly when tunneld's own project label decides it.
	t.Run("scoped to tunneld's own project", func(t *testing.T) {
		composeDaemon(t, "mine",
			composeContainer{id: "mine-web", name: "mine-web-1", project: "mine", service: "web"},
			composeContainer{id: "other-web", name: "other-web-1", project: "other", service: "web"},
		)

		got, err := Open(t.Context(), "web", discard())
		if err != nil {
			t.Fatalf("Open: %v", err)
		}
		defer func() { _ = got.Close() }()
		if got.id != "mine-web" {
			t.Errorf("id = %q, want the web in tunneld's own project (%q)", got.id, "mine-web")
		}
	})

	// Unscoped and ambiguous: picking one silently would point the tunnel at a
	// different replica after a restart, which is a bug nobody would find.
	t.Run("ambiguous service is an error naming the candidates", func(t *testing.T) {
		composeDaemon(t, "",
			composeContainer{id: "a", name: "mine-web-1", project: "mine", service: "web"},
			composeContainer{id: "b", name: "other-web-1", project: "other", service: "web"},
		)

		got, err := Open(t.Context(), "web", discard())
		if err == nil {
			_ = got.Close()
			t.Fatal("Open on an ambiguous service succeeded, want an error")
		}
		if !errors.Is(err, v1.ErrInvalidOrigin) {
			t.Errorf("error = %v, want %v", err, v1.ErrInvalidOrigin)
		}
		for _, want := range []string{"mine-web-1", "other-web-1"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q does not name candidate %q", err, want)
			}
		}
	})

	// Nothing by that name and nothing with that label: the error the operator
	// already knows, unchanged.
	t.Run("no container and no service", func(t *testing.T) {
		composeDaemon(t, "", composeContainer{
			id: "abc", name: "proj-api-1", project: "proj", service: "api",
		})

		got, err := Open(t.Context(), "nope", discard())
		if err == nil {
			_ = got.Close()
			t.Fatal("Open succeeded, want an error")
		}
		if !errors.Is(err, v1.ErrInvalidOrigin) {
			t.Errorf("error = %v, want %v", err, v1.ErrInvalidOrigin)
		}
		if !strings.Contains(err.Error(), "nope") {
			t.Errorf("error %q does not name the reference", err)
		}
	})
}

// TestSelfIDs pins the candidate order mountinfo parsing produces. The ids are
// guesses checked by being inspected, so the job here is to put the likely one
// first and never to invent one.
func TestSelfIDs(t *testing.T) {
	const self = "42a7dbf8b5c82c9bf59749261bf0f8998fdb6f35dc74542168bdc5b5800c21b4"
	const layer = "433163a110ebac893af94c0c0f05ef45501c8ef19a93c814e109034599c5105b"

	cases := []struct {
		name string
		in   string
		want []string
	}{
		{"empty", "", nil},
		{"no hex at all", "24 30 0:22 / /proc rw,nosuid - proc proc rw\n", nil},
		{
			// The shape observed on a real daemon: the runtime's per-container
			// directory is what bind-mounts /etc/hosts.
			name: "the containers path wins over a layer path",
			in: "1 2 0:1 / / rw - overlay overlay rw,upperdir=/var/lib/docker/overlay2/" + layer + "/diff\n" +
				"3 4 0:2 /" + self + "/hostname /etc/hostname rw - ext4 /dev/vda1 rw\n" +
				"5 6 0:3 /var/lib/docker/containers/" + self + "/hosts /etc/hosts rw - ext4 /dev/vda1 rw\n",
			want: []string{self, layer},
		},
		{
			name: "without a containers path, first seen leads",
			in: "1 2 0:1 / / rw - overlay overlay rw,upperdir=/x/" + layer + "/diff\n" +
				"3 4 0:2 /y/" + self + "/hostname /etc/hostname rw - ext4 /dev/vda1 rw\n",
			want: []string{layer, self},
		},
		{"deduplicated", "/containers/" + self + "/a\n/containers/" + self + "/b\n", []string{self}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := selfIDs(strings.NewReader(tc.in))
			if len(got) != len(tc.want) {
				t.Fatalf("selfIDs = %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Errorf("selfIDs[%d] = %s, want %s", i, got[i], tc.want[i])
				}
			}
		})
	}
}

// TestOpenScopesByMountinfo pins the fix for #28: a service that sets its own
// hostname — which network_mode: service:<name> requires, so it is the shape
// this feature exists to serve — used to defeat the project lookup entirely,
// because the hostname then names nothing the daemon knows. The container id
// is still in mountinfo, and scoping has to survive on it.
func TestOpenScopesByMountinfo(t *testing.T) {
	const selfID = "42a7dbf8b5c82c9bf59749261bf0f8998fdb6f35dc74542168bdc5b5800c21b4"

	dir := t.TempDir()
	path := filepath.Join(dir, "mountinfo")
	body := "5 6 0:3 /var/lib/docker/containers/" + selfID + "/hosts /etc/hosts rw - ext4 /dev/vda1 rw\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write mountinfo: %v", err)
	}
	old := mountinfo
	mountinfo = path
	t.Cleanup(func() { mountinfo = old })

	// selfProject "" so the hostname resolves to nothing, exactly as it does
	// for a container whose hostname was overridden.
	composeDaemon(t, "",
		composeContainer{id: selfID, name: "mine-tunneld-1", project: "mine", service: "tunneld"},
		composeContainer{id: "mine-cc", name: "mine-claude-code-1", project: "mine", service: "claude-code"},
		composeContainer{id: "other-cc", name: "other-claude-code-1", project: "other", service: "claude-code"},
	)

	got, err := Open(t.Context(), "claude-code", discard())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer func() { _ = got.Close() }()
	if got.id != "mine-cc" {
		t.Errorf("id = %q, want the claude-code in tunneld's own project (%q)", got.id, "mine-cc")
	}
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
