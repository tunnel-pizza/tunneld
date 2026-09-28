// The tests for pid.go. `package pid_test` is the outside view: the files a
// run leaves and the launcher reads are the contract, not anything unexported.
package pid_test

import (
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

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
	o := origins.New(origins.WithDir("/work/project"), origins.WithURL(u))
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

// TestNewTakesTheVariableOut pins that the launcher's pid is read once and
// not left for a program an origin starts to inherit.
func TestNewTakesTheVariableOut(t *testing.T) {
	t.Setenv(v1.NotifyPidEnv, strconv.Itoa(os.Getppid()))
	pid.New()
	if v, ok := os.LookupEnv(v1.NotifyPidEnv); ok {
		t.Errorf("%s = %q after New, want it unset", v1.NotifyPidEnv, v)
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
	for _, want := range []string{"as pid " + mine, "npx tunneld -k ends it"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
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

// TestNothingToDo pins the quiet cases: no directory registers nothing and
// fails nothing, and with no launcher waiting Detach says so and touches
// nothing.
func TestNothingToDo(t *testing.T) {
	_, o, _ := fixed(t)
	release, err := pid.New(pid.WithDir("")).Register(o, discard())
	if err != nil {
		t.Fatalf("Register() with no directory = %v, want nothing", err)
	}
	release()

	p, o, stem := fixed(t, pid.WithParent(0))
	if p.Detach(o, discard()) {
		t.Error("Detach() = true with no launcher waiting")
	}
	if _, err := os.Stat(stem + ".log"); !os.IsNotExist(err) {
		t.Errorf("log: %v, want none", err)
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
