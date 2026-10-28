package shell

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"

	ltv1 "github.com/cnuss/libtunnel/v1"
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

// needsPTY skips a test on a machine with no pseudo-terminals: Windows, the
// one in the CI matrix, where creack/pty compiles and refuses at run time, or
// a sandbox with no devpts. Asked the way Open asks, by opening a pair. What
// such a machine does instead — pipes — is pipes_test.go's.
func needsPTY(t *testing.T) {
	t.Helper()
	master, slave, err := openPTY()
	if err != nil {
		t.Skipf("no pseudo-terminals on this machine: %v", err)
	}
	_ = slave.Close()
	_ = master.Close()
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
		_, err := New().Open(t.Context(), "tunneld-absent-fixture", nil, log)
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

		target, err := New().Open(t.Context(), runnable("tunneld-fixture"), nil, log)
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

	target, err := New().Open(t.Context(), path, nil, slog.New(slog.DiscardHandler))
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

	target, err := New().Open(t.Context(), path, nil, slog.New(slog.DiscardHandler))
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

	target, err := New().Open(t.Context(), path, nil, slog.New(slog.DiscardHandler))
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

	target, err := New().Open(t.Context(), path, nil, slog.New(slog.DiscardHandler))
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

// TestAttachPassesTheArguments pins that what Open was handed reaches the
// program's argv, in order and whole — a space inside one argument stays inside
// it, which is the promise a shell makes and the query encoding has to keep.
func TestAttachPassesTheArguments(t *testing.T) {
	needsPTY(t)

	dir := t.TempDir()
	path := filepath.Join(dir, "tunneld-args-fixture")
	script := "#!/bin/sh\nprintf 'ARGS:%s|%s|%s\\n' \"$1\" \"$2\" \"$3\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	target, err := New().Open(t.Context(), path, []string{"--resume", "two words", "-d"}, slog.New(slog.DiscardHandler))
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
	if got := out.String(); !strings.Contains(got, "ARGS:--resume|two words|-d") {
		t.Errorf("attach wrote %q, want the arguments in order", got)
	}

	// The origin the frame shows names the program and not how it was
	// started: the query is the carrier, and a person is shown the path.
	if got, want := target.Origin(), string(v1.ExecScheme)+"://"+path; got != want {
		t.Errorf("Origin() = %q, want %q", got, want)
	}
}

// TestEndStopsTheProgramAndWhatItStarted pins End against the case that made
// it necessary: a program with a child of its own, a dev server and its
// worker. Ending the program alone would leave the child holding the port;
// End signals the group, and the attach returns with nothing left running.
//
// The child ignores SIGHUP. When a session leader exits the terminal hangs up
// on its foreground group, which would end an ordinary child on its own and
// prove nothing about End; a child that shrugs the hangup off — a worker that
// was nohup'd, a daemon — is the one only the group signal reaches.
func TestEndStopsTheProgramAndWhatItStarted(t *testing.T) {
	needsPTY(t)

	dir := t.TempDir()
	path := write(t, dir, runnable("tunneld-parent"), 0o755)
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntrap '' HUP\nsleep 31337 &\nwait\n"), 0o755); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}

	target, err := New().Open(t.Context(), path, nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer func() { _ = target.Close() }()

	resize := make(chan remotecommand.TerminalSize)
	close(resize)
	attached := make(chan error, 1)
	go func() {
		attached <- target.AttachContainer(t.Context(), "", "", "", strings.NewReader(""), &sink{}, &sink{}, true, resize)
	}()
	// Give the shell a moment to start its child.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if out, _ := exec.Command("pgrep", "-f", "sleep 31337").Output(); len(out) > 0 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	ender, ok := target.(attach.Ender)
	if !ok {
		t.Fatal("the shell target does not implement Ender")
	}
	if err := ender.End(t.Context()); err != nil {
		t.Fatalf("End() = %v", err)
	}
	select {
	case <-attached:
	case <-time.After(3 * time.Second):
		t.Fatal("the attach did not return after End")
	}
	if out, _ := exec.Command("pgrep", "-f", "sleep 31337").Output(); len(out) > 0 {
		t.Errorf("the program's child is still running after End: pids %s", strings.TrimSpace(string(out)))
	}
}

// TestAttachKeepsThePasswordFromTheProgram pins that a program never
// inherits the tunnel's password hash, on a terminal or over pipes, while
// the rest of tunneld's environment still reaches it.
func TestAttachKeepsThePasswordFromTheProgram(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the fixture is a /bin/sh script")
	}
	t.Setenv(v1.WWWAuthenticateEnv, `Basic pw="$pbkdf2-sha256$i=600000$c2FsdA$aGFzaA"`)
	t.Setenv(ltv1.TokenEnv, "a.mint.token")
	t.Setenv("TUNNELD_ENV_FIXTURE", "kept")
	path := filepath.Join(t.TempDir(), "tunneld-env-fixture")
	script := "#!/bin/sh\nprintf 'AUTH:%s|TOKEN:%s|KEPT:%s\\n' \"${TUNNELD_WWW_AUTHENTICATE-unset}\" \"${LIBTUNNEL_TOKEN-unset}\" \"$TUNNELD_ENV_FIXTURE\"\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("writing the fixture: %v", err)
	}
	for name, pipes := range map[string]bool{"on a terminal": false, "over pipes": true} {
		t.Run(name, func(t *testing.T) {
			if !pipes {
				needsPTY(t)
			}
			target, err := New().Open(t.Context(), path, nil, slog.New(slog.DiscardHandler))
			if err != nil {
				t.Fatalf("Open() = %v", err)
			}
			defer func() { _ = target.Close() }()
			target.(*TargetImpl).pipes = pipes

			out := &sink{}
			resize := make(chan remotecommand.TerminalSize)
			close(resize)
			if err := target.AttachContainer(t.Context(), "", "", "", strings.NewReader(""), out, out, true, resize); err != nil {
				t.Fatalf("AttachContainer() = %v", err)
			}
			if got := out.String(); !strings.Contains(got, "AUTH:unset|TOKEN:unset|KEPT:kept") {
				t.Errorf("the program saw %q, want no password, no mint token, and the rest kept", got)
			}
		})
	}
}

// TestEnviron pins the environment a program starts from: tunneld's own,
// without the tunnel's password, under any spelling a case-blind
// environment would read as it.
func TestEnviron(t *testing.T) {
	t.Setenv(v1.WWWAuthenticateEnv, "Basic pw=x")
	t.Setenv(ltv1.TokenEnv, "a.mint.token")
	t.Setenv("TUNNELD_ENV_FIXTURE", "kept")
	got := environ()
	if !slices.Contains(got, "TUNNELD_ENV_FIXTURE=kept") {
		t.Errorf("environ() lost the rest of the environment: %q", got)
	}
	for _, kv := range got {
		if name, _, _ := strings.Cut(kv, "="); strings.EqualFold(name, v1.WWWAuthenticateEnv) || strings.EqualFold(name, ltv1.TokenEnv) {
			t.Errorf("environ() kept %q", kv)
		}
	}
	if runtime.GOOS != "windows" {
		t.Setenv(strings.ToLower(v1.WWWAuthenticateEnv), "Basic pw=y")
		for _, kv := range environ() {
			if name, _, _ := strings.Cut(kv, "="); strings.EqualFold(name, v1.WWWAuthenticateEnv) {
				t.Errorf("environ() kept %q", kv)
			}
		}
	}
}
