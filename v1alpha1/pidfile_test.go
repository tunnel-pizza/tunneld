package v1alpha1

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

// TestRunDir pins where a run registers, which the npm launcher's runDir
// resolves the same way: $XDG_RUNTIME_DIR/tunneld when it is set and
// absolute, the cache directory's run otherwise.
func TestRunDir(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	if got, want := runDir(), filepath.Join(runtimeDir, "tunneld"); got != want {
		t.Errorf("with $XDG_RUNTIME_DIR: runDir() = %q, want %q", got, want)
	}

	for _, v := range []string{"", "relative/dir"} {
		t.Setenv("XDG_RUNTIME_DIR", v)
		base, err := os.UserCacheDir()
		if err != nil {
			t.Skipf("no user cache dir here: %v", err)
		}
		if got, want := runDir(), filepath.Join(base, "tunneld", "run"); got != want {
			t.Errorf("$XDG_RUNTIME_DIR=%q: runDir() = %q, want %q", v, got, want)
		}
	}
}

// TestRegister pins the file: named by the pid, carrying it, held open until
// release, and gone after. Held open is what the launcher's -k tells a live
// run by, so it is checked the way -k checks it where that is cheap: Linux,
// through /proc.
func TestRegister(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "run")
	release := register(dir, slog.New(slog.NewTextHandler(io.Discard, nil)))
	file := filepath.Join(dir, strconv.Itoa(os.Getpid()))

	got, err := os.ReadFile(file)
	if err != nil {
		t.Fatalf("reading %s: %v", file, err)
	}
	if want := strconv.Itoa(os.Getpid()) + "\n"; string(got) != want {
		t.Errorf("%s = %q, want %q", file, got, want)
	}
	if info, err := os.Stat(dir); err == nil && runtime.GOOS != "windows" && info.Mode().Perm() != 0o700 {
		t.Errorf("%s mode = %v, want 0700", dir, info.Mode().Perm())
	}
	if runtime.GOOS == "linux" && !heldBySelf(t, file) {
		t.Errorf("%s is not held open by this process", file)
	}

	release()
	if _, err := os.Stat(file); !os.IsNotExist(err) {
		t.Errorf("%s after release: %v, want it removed", file, err)
	}
	if runtime.GOOS == "linux" && heldBySelf(t, file) {
		t.Errorf("%s is still held open after release", file)
	}
}

// TestRegisterFailsQuietly pins that a run which cannot register still runs:
// the failure is a warning, and release is a no-op.
func TestRegisterFailsQuietly(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	var logged bytes.Buffer
	release := register(filepath.Join(blocker, "run"), slog.New(slog.NewTextHandler(&logged, nil)))
	release()
	if !strings.Contains(logged.String(), "not registering this run") {
		t.Errorf("log = %q, want the failure warned about", logged.String())
	}

	register("", slog.New(slog.NewTextHandler(io.Discard, nil)))() // no directory: nothing, and no panic
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
