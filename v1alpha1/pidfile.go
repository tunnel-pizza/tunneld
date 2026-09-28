package v1alpha1

import (
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
)

// runDir is where a run registers itself: $XDG_RUNTIME_DIR/tunneld where the
// session has one, which is what that directory is for — per user, 0700, and
// emptied at reboot along with every process a file there could name — and
// <user cache dir>/tunneld/run otherwise, which is macOS, and Linux without
// logind: a container, some ssh sessions. The npm launcher resolves the same
// two in the same order. Empty when this machine has neither.
func runDir() string {
	if dir := os.Getenv("XDG_RUNTIME_DIR"); filepath.IsAbs(dir) {
		return filepath.Join(dir, "tunneld")
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(base, "tunneld", "run")
}

// register writes this process's pid to a file named after it in dir, and
// keeps the file open until the returned release is called, which closes and
// removes it. The npm launcher's -k finds every run by these files and ends
// each one with SIGINT.
//
// Keeping it open is the fingerprint. A pid on its own names whatever the
// system has since handed that number to; a file this process holds open is
// closed by the kernel however the process ends, kill -9 included, so a
// process holding its own file is a live run and nothing else can be. -k asks
// which process holds the file rather than trusting the name. Go opens files
// close-on-exec, so a program an origin starts does not inherit it.
//
// A run that cannot register still runs: the file is how another command
// finds it, not something the tunnel needs. The failure is logged, and
// release is then a no-op.
func register(dir string, log *slog.Logger) (release func()) {
	release = func() {}
	if dir == "" {
		return release
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		log.Warn("not registering this run", "err", err)
		return release
	}
	path := filepath.Join(dir, strconv.Itoa(os.Getpid()))
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		log.Warn("not registering this run", "err", err)
		return release
	}
	if _, err := f.WriteString(strconv.Itoa(os.Getpid()) + "\n"); err != nil {
		log.Warn("not registering this run", "err", err)
		f.Close()
		os.Remove(path)
		return release
	}
	log.Debug("registered this run", "path", path)
	return func() {
		f.Close()
		os.Remove(path)
	}
}
