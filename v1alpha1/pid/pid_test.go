// The tests for pid.go. `package pid_test` is the outside view: the files a
// run leaves and the launcher reads are the contract, not anything unexported.
package pid_test

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"maps"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/origins"
	"github.com/tunnel-pizza/tunneld/v1alpha1/pid"
)

func discard() *slog.Logger { return slog.New(slog.DiscardHandler) }

// fixed is a PidImpl in a directory only this case can see, a run, and the
// path its files share before the extension.
func fixed(t *testing.T, opts ...pid.Option) (*pid.PidImpl, v1.Origins, string) {
	t.Helper()
	dir := t.TempDir()
	u, _ := url.Parse("http://localhost:3000")
	o := origins.New(origins.WithDir("/work/project"), origins.WithOrigins(origins.NewOrigin(u)))
	return pid.New(append([]pid.Option{pid.WithDir(dir)}, opts...)...), o, filepath.Join(dir, o.Key())
}

// TestParent pins the gate on the signal: only a value naming this process's
// own parent is one to signal. SIGUSR2 ends a process that has not asked for
// it, so anything else — a shell, a supervisor, a value inherited from
// somewhere — is left alone.
func TestParent(t *testing.T) {
	const ppid = 4242
	for _, tc := range []struct {
		value string
		want  int
	}{
		{strconv.Itoa(ppid), ppid},
		{"", 0},
		{"4243", 0},
		{"launcher", 0},
		{"1", 0},
		{"-4242", 0},
	} {
		if got := pid.Parent(tc.value, ppid); got != tc.want {
			t.Errorf("Parent(%q, %d) = %d, want %d", tc.value, ppid, got, tc.want)
		}
	}
}

// TestNewTakesTheVariableOut pins that the launcher's status is read once and
// not left for a program an origin starts to inherit.
func TestNewTakesTheVariableOut(t *testing.T) {
	t.Setenv(v1.NotifyPidEnv, strconv.Itoa(os.Getppid()))
	pid.New()
	if v, ok := os.LookupEnv(v1.NotifyPidEnv); ok {
		t.Errorf("%s = %q after New, want it unset", v1.NotifyPidEnv, v)
	}
}

// TestNewTakesTheStatusPipeOnlyFromALauncher pins that the status
// pipe is the launcher's protocol, like the signal: honoured only
// beside a NotifyPidEnv naming this process's parent, and taken out of the
// environment either way.
func TestNewTakesTheStatusPipeOnlyFromALauncher(t *testing.T) {
	t.Setenv(v1.StatusFdEnv, "3")
	pid.New()
	if v, ok := os.LookupEnv(v1.StatusFdEnv); ok {
		t.Errorf("%s = %q after New, want it unset", v1.StatusFdEnv, v)
	}
}

// TestRegister pins a run's registration: <key>.pid, naming the pid, held
// open until release, and gone after. Held open is what the npm launcher's -k
// tells a live run by, so it is checked the way -k checks it where that is
// cheap: Linux, through /proc.
func TestRegister(t *testing.T) {
	p, o, stem := fixed(t)
	path := stem + ".pid"
	want := strconv.Itoa(os.Getpid()) + "\n"

	release, err := p.Register(o, discard())
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if got, err := os.ReadFile(path); err != nil || string(got) != want {
		t.Fatalf("%s = %q (%v), want %q", path, got, err, want)
	}
	if runtime.GOOS == "linux" && !heldBySelf(t, path) {
		t.Errorf("%s is not held open by this process", path)
	}
	release()
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("%s after release: %v, want it removed", path, err)
	}
	if runtime.GOOS == "linux" && heldBySelf(t, path) {
		t.Errorf("%s is still held open after release", path)
	}
}

// TestRegisterRefusesTheSameRun pins one run to a key: the same run started
// again while the first is going is refused with ErrRunning, naming the
// first's pid, and leaves the first's registration as it was. Once the first
// has gone, the next registers. The lock is the open file's, so two
// registrations in one process contend the way two processes do.
func TestRegisterRefusesTheSameRun(t *testing.T) {
	p, o, stem := fixed(t)
	path := stem + ".pid"
	first, err := p.Register(o, discard())
	if err != nil {
		t.Fatalf("first Register() error = %v", err)
	}
	mine := strconv.Itoa(os.Getpid())

	release, err := p.Register(o, discard())
	if !errors.Is(err, v1.ErrRunning) || release != nil {
		t.Fatalf("second Register() = %v, want ErrRunning and no release", err)
	}
	if want := "already running as pid " + mine; err.Error() != want {
		t.Errorf("error %q, want %q and nothing of the launcher's", err, want)
	}
	if exit := (v1.ExitError{}); !errors.As(err, &exit) || exit.Code != v1.RunningCode {
		t.Errorf("error %v, want an ExitError with status %d", err, v1.RunningCode)
	}
	if got, _ := os.ReadFile(path); string(got) != mine+"\n" {
		t.Errorf("%s after the refusal = %q, want the first run's pid untouched", path, got)
	}

	first()
	again, err := p.Register(o, discard())
	if err != nil {
		t.Fatalf("Register() after the first ended = %v, want it registered", err)
	}
	again()
}

// TestRegisterAnswersTheLauncher pins what a launcher waiting on the status
// pipe reads: a refused run writes a line of JSON naming the pid it was
// refused for and that run's settings and log files, those that exist, and
// any run closes the pipe once Register returns, so the launcher reads to the
// end either way and a detached run holds nothing of its.
func TestRegisterAnswersTheLauncher(t *testing.T) {
	p, o, stem := fixed(t)
	first, err := p.Register(o, discard())
	if err != nil {
		t.Fatalf("first Register() error = %v", err)
	}
	refused := func() map[string]any {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatal(err)
		}
		_, err = pid.New(pid.WithDir(filepath.Dir(stem)), pid.WithStatusPipe(w)).Register(o, discard())
		if !errors.Is(err, v1.ErrRunning) {
			t.Fatalf("second Register() = %v, want ErrRunning", err)
		}
		line := readAll(t, r)
		var got map[string]any
		if err := json.Unmarshal([]byte(line), &got); err != nil || !strings.HasSuffix(line, "}\n") {
			t.Fatalf("status pipe after a refusal = %q, want a line of JSON: %v", line, err)
		}
		return got
	}

	if got, want := refused(), map[string]any{"pid": float64(os.Getpid())}; !maps.Equal(got, want) {
		t.Errorf("status with no settings or log = %v, want %v", got, want)
	}
	for _, ext := range []string{".env", ".log"} {
		if err := os.WriteFile(stem+ext, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	want := map[string]any{"pid": float64(os.Getpid()), "env": stem + ".env", "log": stem + ".log"}
	if got := refused(); !maps.Equal(got, want) {
		t.Errorf("status = %v, want %v", got, want)
	}
	first()

	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	release, err := pid.New(pid.WithDir(filepath.Dir(stem)), pid.WithStatusPipe(w)).Register(o, discard())
	if err != nil {
		t.Fatalf("Register() = %v, want it registered", err)
	}
	defer release()
	if got := readAll(t, r); got != "" {
		t.Errorf("status pipe after registering = %q, want it closed with nothing written", got)
	}
}

func readAll(t *testing.T, r *os.File) string {
	t.Helper()
	defer r.Close()
	// Windows pipes take no deadline; go test's own timeout covers them.
	if err := r.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil && !errors.Is(err, os.ErrNoDeadline) {
		t.Fatal(err)
	}
	b, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("reading the status pipe: %v", err)
	}
	return string(b)
}

// TestNothingToDo pins the quiet cases: no directory registers nothing and
// fails nothing, and with no launcher waiting Detach says so and moves
// nothing.
func TestNothingToDo(t *testing.T) {
	_, o, _ := fixed(t)
	release, err := pid.New(pid.WithDir("")).Register(o, discard())
	if err != nil {
		t.Fatalf("Register() with no directory = %v, want nothing", err)
	}
	release()

	p, _, _ := fixed(t, pid.WithParent(0))
	if p.Detach(nil, discard()) {
		t.Error("Detach() = true with no launcher waiting")
	}
}

func heldBySelf(t *testing.T, file string) bool {
	t.Helper()
	fds, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		t.Fatalf("reading /proc/self/fd: %v", err)
	}
	for _, fd := range fds {
		if target, err := os.Readlink(filepath.Join("/proc/self/fd", fd.Name())); err == nil && target == file {
			return true
		}
	}
	return false
}
