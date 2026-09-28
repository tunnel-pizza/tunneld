// Package pid is how a run is found and handed back from outside it.
//
// Two things, both for the npm launcher. A run registers itself as <key>.pid
// beside its cached spec, a file it holds open for as long as it runs, which
// is what the launcher's -k finds every run by. And a run the launcher
// detached tells it, once its addresses are out, that it can hand the console
// back: it moves its stdout and stderr to <key>.log and signals its parent.
package pid

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// dirName is the directory under the user's cache directory, the one the
// spec cache files into, so a run's three files — spec, pid, log — sit
// together under one name.
const dirName = "tunneld"

// Option configures a PidImpl at construction.
type Option = v1.Option[*PidImpl]

// PidImpl is the default: files in the user's cache directory, and a parent
// to signal when v1.NotifyPidEnv names it.
type PidImpl struct {
	// dir is where the files go. Empty means this machine has no cache
	// directory, and nothing is registered or logged.
	dir string

	// parent is the launcher waiting on this run, or 0 for none.
	parent int
}

// New returns a PidImpl configured by opts, pointed at the user's cache
// directory. It reads v1.NotifyPidEnv once and takes it out of the
// environment, so no program an origin starts inherits it.
func New(opts ...Option) *PidImpl {
	p := &PidImpl{parent: Parent(os.Getenv(v1.NotifyPidEnv), os.Getppid())}
	os.Unsetenv(v1.NotifyPidEnv)
	if base, err := os.UserCacheDir(); err == nil {
		p.dir = filepath.Join(base, dirName)
	}
	return v1.Apply(p, opts...)
}

// WithDir replaces the directory the files go in — a temporary directory in a
// test. Empty turns registering and logging off.
func WithDir(dir string) Option {
	return func(p *PidImpl) { p.dir = dir }
}

// WithParent replaces the launcher to signal, which New reads from the
// environment. 0 is none.
func WithParent(pid int) Option {
	return func(p *PidImpl) { p.parent = pid }
}

// Parent is the pid to signal once the addresses are out, or 0 for none: the
// value of v1.NotifyPidEnv, when it names this process's own parent.
// Anything else — unset, not a number, or some other process — is no, since
// SIGUSR2 ends a process that did not ask for it, and whatever started a run
// is not always a launcher.
func Parent(value string, ppid int) int {
	pid, err := strconv.Atoi(value)
	if err != nil || pid <= 1 || pid != ppid {
		return 0
	}
	return pid
}

func (p *PidImpl) file(origins v1.Origins, ext string) string {
	if p.dir == "" || origins == nil {
		return ""
	}
	return filepath.Join(p.dir, origins.Key()+ext)
}

// Register marks this run as running, as <key>.pid: the file names the pid,
// and this process holds it open until release is called, which closes it and
// removes it. The npm launcher's -k ends every process holding one with
// SIGINT.
//
// Held open is the fingerprint. A pid on its own names whatever the system has
// since handed that number to; a file a process holds open is closed by the
// kernel however the process ends, kill -9 included, so a process holding one
// of these is a live run and nothing else can be. -k asks which processes hold
// the file rather than trusting what it says. Go opens files close-on-exec, so
// a program an origin starts does not inherit it.
//
// Two runs of the same thing at once share the file, and -k ends every process
// holding it; release removes it only while it still names this pid, so the
// first of the two to finish does not unregister the other. Only when the
// later one finishes first is the earlier left unfindable.
//
// A run that cannot register still runs: the file is how another command
// finds it, not something the tunnel needs. The failure is logged and release
// is then a no-op.
func (p *PidImpl) Register(origins v1.Origins, log v1.Logger) (release func()) {
	release = func() {}
	path := p.file(origins, ".pid")
	if path == "" {
		return release
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Warn("not registering this run", "error", err)
		return release
	}
	pid := strconv.Itoa(os.Getpid()) + "\n"
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		log.Warn("not registering this run", "error", err)
		return release
	}
	if _, err := f.WriteString(pid); err != nil {
		log.Warn("not registering this run", "error", err)
		f.Close()
		return release
	}
	log.Debug("registered this run", "path", path)
	return func() {
		if body, err := os.ReadFile(path); err == nil && string(body) == pid {
			os.Remove(path)
		}
		f.Close()
	}
}

// Detach hands a run back from the launcher waiting on it, and reports
// whether there was one. The addresses and the banner have gone to the
// launcher's streams, which are the caller's; what the run says from here
// goes to <key>.log, emptied, or nowhere when there is no directory to keep
// one in. The last line on the caller's stderr says which, then stdout and
// stderr are moved and the launcher is signalled, in that order, so that
// once it exits nothing here holds a stream of the caller's: `$(npx tunneld
// -d …)` returns, and nothing lands on a prompt later.
//
// A run that cannot move its streams still signals: the launcher exits either
// way, and the caller's streams stay held until the run ends, which is logged.
func (p *PidImpl) Detach(origins v1.Origins, stderr io.Writer, log v1.Logger) bool {
	if p.parent == 0 {
		return false
	}
	var out *os.File
	if path := p.file(origins, ".log"); path != "" {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			log.Warn("no log for this detached run", "error", err)
		} else {
			fmt.Fprintf(stderr, "%s: detached; its log is %s\n", v1.CommandName, path)
			out = f
		}
	}
	if out == nil {
		f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			log.Warn("cannot let go of the caller's streams", "error", err)
		}
		out = f
	}
	if out != nil {
		if err := redirect(out); err != nil {
			log.Warn("cannot let go of the caller's streams", "error", err)
		}
		out.Close()
	}
	if err := notify(p.parent); err != nil {
		log.Warn("cannot tell the launcher this run is up", "pid", p.parent, "error", err)
	}
	return true
}
