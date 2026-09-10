package shell

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// runnable names a file the host will actually agree to run. Unix reads the
// mode bit and Windows reads the extension, so the same fixture needs a
// suffix on one and not the other; .bat is on the default PATHEXT that
// exec.LookPath falls back to when the variable is unset.
func runnable(base string) string {
	if runtime.GOOS == "windows" {
		return base + ".bat"
	}
	return base
}

// needsPTY skips a test on a platform with no pseudo-terminals. Windows is the
// one in the CI matrix: creack/pty compiles there and refuses at run time, so
// everything below Open's probe is unreachable rather than broken.
func needsPTY(t *testing.T) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("no pseudo-terminals on this platform")
	}
}

// sink is an out or errw for AttachContainer: a buffer that can be closed and
// read from another goroutine, since the copy writes to it while the test
// waits.
type sink struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (s *sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *sink) Close() error { return nil }

func (s *sink) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

// TestResolve pins what turns a bare argument into a program origin, and what
// the origin then carries. The rules belong to the host — the $PATH search,
// the executable bit, the Windows extensions — so the fixture is a directory
// made the whole of $PATH and the cases are what an operator can type at it.
func TestResolve(t *testing.T) {
	dir := t.TempDir()
	prog := write(t, dir, runnable("tunneld-fixture"), 0o755)
	write(t, dir, "tunneld-plain", 0o644)
	t.Setenv("PATH", dir)

	cases := []struct {
		name string
		in   string
		want bool
	}{
		{"a command on PATH", runnable("tunneld-fixture"), true},
		{"the same command by path", prog, true},
		// Not runnable for a different reason on each platform — no executable
		// bit on Unix, no PATHEXT extension on Windows — and the answer is the
		// one that matters either way.
		{"a file on PATH that is not runnable", "tunneld-plain", false},
		{"a name that is not there", "tunneld-absent-fixture", false},
		// The parser asks this before the http:// default, so anything already
		// carrying a scheme has to answer no: LookPath stats it as a path.
		{"a URL", "http://localhost:3000", false},
		{"a host and port", "localhost:3000", false},
		{"a bare port", ":8000", false},
		{"a container reference", "attach://dockerd/api", false},
		{"nothing at all", "", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path, got := Resolve(tc.in)
			if got != tc.want {
				t.Errorf("Resolve(%q) = %v, want %v", tc.in, got, tc.want)
			}
			// What resolves resolves to the one absolute path that will run,
			// whichever spelling was typed at it.
			if got && path != prog {
				t.Errorf("Resolve(%q) = %q, want the resolved path %q", tc.in, path, prog)
			}
			if !got && path != "" {
				t.Errorf("Resolve(%q) = %q alongside false, want no path", tc.in, path)
			}
		})
	}
}

// write drops a fixture into dir and returns its path. The body is a shell
// script that says one word, which is what the attach test reads back.
func write(t *testing.T, dir, name string, mode os.FileMode) string {
	t.Helper()
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\necho TUNNELD_FIXTURE_OK\n"), mode); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
	return path
}

// TestOpenResolves pins that everything knowable about a program origin is
// settled before the tunnel is minted: a name that resolves to nothing is
// refused as an invalid origin, naming the value, rather than becoming a page
// that answers an error to whoever was handed the URL.
func TestOpenResolves(t *testing.T) {
	log := slog.New(slog.DiscardHandler)

	t.Run("a name that is not there", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		_, err := New().Open(t.Context(), "tunneld-absent-fixture", log)
		if !errors.Is(err, v1.ErrInvalidOrigin) {
			t.Fatalf("Open() = %v, want ErrInvalidOrigin", err)
		}
		if !strings.Contains(err.Error(), "tunneld-absent-fixture") {
			t.Errorf("error %q does not name the value", err)
		}
	})

	t.Run("a program on PATH", func(t *testing.T) {
		needsPTY(t)
		dir := t.TempDir()
		write(t, dir, runnable("tunneld-fixture"), 0o755)
		t.Setenv("PATH", dir)

		target, err := New().Open(t.Context(), runnable("tunneld-fixture"), log)
		if err != nil {
			t.Fatalf("Open() = %v", err)
		}
		defer func() { _ = target.Close() }()

		// The reference the operator typed, not the resolved path: it is what
		// they recognize in a page title.
		if got, want := target.Name(), runnable("tunneld-fixture"); got != want {
			t.Errorf("Name() = %q, want %q", got, want)
		}
		if !target.TTY() || !target.Stdin() {
			t.Errorf("TTY() = %v, Stdin() = %v, want both true — the origin is a terminal", target.TTY(), target.Stdin())
		}
	})
}

// TestAttachRunsTheProgram pins the whole point of the provider: the program
// runs when somebody attaches, and what it prints reaches the caller's stream.
// Nothing runs before that — Open resolves and probes, and a target nobody
// attaches to costs nothing.
func TestAttachRunsTheProgram(t *testing.T) {
	needsPTY(t)

	dir := t.TempDir()
	path := write(t, dir, runnable("tunneld-fixture"), 0o755)

	target, err := New().Open(t.Context(), path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer func() { _ = target.Close() }()

	out := &sink{}
	resize := make(chan remotecommand.TerminalSize)
	close(resize)

	if err := target.AttachContainer(t.Context(), "", "", "", strings.NewReader(""), out, &sink{}, true, resize); err != nil {
		t.Fatalf("AttachContainer() = %v", err)
	}
	if got := out.String(); !strings.Contains(got, "TUNNELD_FIXTURE_OK") {
		t.Errorf("attach wrote %q, want the program's own output", got)
	}
}

// TestResizeStopsWithTheAttach pins the contract a provider owes a caller
// whose resize channel outlives one attach.
//
// The session hands the same channel to every run it starts, so a reader that
// only stopped when the channel closed would go on taking sizes meant for
// whatever ran next. A size taken by a terminal that is already gone is a size
// the running program never hears — and for a full-screen program that is a
// pty left at nothing and a page left blank, with the program plainly running.
func TestResizeStopsWithTheAttach(t *testing.T) {
	needsPTY(t)

	dir := t.TempDir()
	path := write(t, dir, runnable("tunneld-fixture"), 0o755)

	target, err := New().Open(t.Context(), path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer func() { _ = target.Close() }()

	// Unbuffered, so a send succeeds only if something is actually reading.
	resize := make(chan remotecommand.TerminalSize)
	if err := target.AttachContainer(t.Context(), "", "", "", strings.NewReader(""), &sink{}, &sink{}, true, resize); err != nil {
		t.Fatalf("AttachContainer() = %v", err)
	}

	select {
	case resize <- remotecommand.TerminalSize{Width: 80, Height: 24}:
		t.Error("a size was taken after the attach ended; the next run will never hear it")
	case <-time.After(500 * time.Millisecond):
	}
}

// TestCloseToleratesASecondCall pins the contract Target.Close states: Server
// closes its target unconditionally from both the AfterFunc registered in
// Serve and a caller's own defer, so a target that cannot survive it breaks at
// shutdown.
func TestCloseToleratesASecondCall(t *testing.T) {
	needsPTY(t)

	dir := t.TempDir()
	path := write(t, dir, runnable("tunneld-fixture"), 0o755)

	target, err := New().Open(t.Context(), path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	for i := range 2 {
		if err := target.Close(); err != nil {
			t.Errorf("Close() #%d = %v, want nil", i+1, err)
		}
	}
}

// TestAttachAfterCloseStartsNothing pins that a target closed while an attach
// was starting does not leave a process behind for nobody to kill. The close
// wins by happening first here, which is the same order the race can produce.
func TestAttachAfterCloseStartsNothing(t *testing.T) {
	needsPTY(t)

	dir := t.TempDir()
	path := write(t, dir, runnable("tunneld-fixture"), 0o755)

	target, err := New().Open(t.Context(), path, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	if err := target.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}

	resize := make(chan remotecommand.TerminalSize)
	close(resize)
	out := &sink{}
	if err := target.AttachContainer(t.Context(), "", "", "", strings.NewReader(""), out, &sink{}, true, resize); err != nil {
		t.Fatalf("AttachContainer() after Close = %v, want nil", err)
	}
	if got := out.String(); got != "" {
		t.Errorf("attach after Close wrote %q, want nothing", got)
	}
}
