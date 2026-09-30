// Package pid is how a run is found and handed back from outside it.
//
// Two things, both for the npm launcher. A run registers itself as <key>.pid
// beside its cached spec, a file it holds open for as long as it runs, which
// is what the launcher's -k finds every run by. And a run the launcher
// detached tells it, once its addresses are out, that it can hand the console
// back: it moves its stdout and stderr off the caller's — to the run's log
// file, which v1alpha1/logs keeps — and signals its parent.
package pid

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// dirName is the directory under the user's cache directory, the one the
// spec cache files into, so a run's three files — spec, pid, log — sit
// together under one name.
const dirName = "tunneld"

// errLocked is what lock returns when another process holds the lock.
var errLocked = errors.New("locked by another process")

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
// and this process holds it open, and locked, until release is called, which
// removes it and closes it. The npm launcher's -k ends every process holding
// one with SIGINT.
//
// Held open is the fingerprint. A pid on its own names whatever the system has
// since handed that number to; a file a process holds open is closed by the
// kernel however the process ends, kill -9 included, so a process holding one
// of these is a live run and nothing else can be. -k asks which processes hold
// the file rather than trusting what it says. Go opens files close-on-exec, so
// a program an origin starts does not inherit it.
//
// Locked is what keeps it one run to a file. The same run started again — the
// same directory, origins and arguments, so the same key — finds the lock
// taken and is refused with v1.ErrRunning, naming the pid it read, before it
// mints anything: two of them would answer on one hostname, and one could end
// and take the registration of the other with it. The lock is released by the
// kernel as the file is closed, however that happens, so a run that died
// leaves nothing to refuse the next. The file is opened without truncating and
// only written once the lock is held, so a refused run never erases the pid
// of the one it was refused for.
//
// A run that cannot register for any other reason still runs: the file is how
// another command finds it, not something the tunnel needs. The failure is
// logged and release is then a no-op.
func (p *PidImpl) Register(origins v1.Origins, log v1.Logger) (release func(), err error) {
	release = func() {}
	path := p.file(origins, ".pid")
	if path == "" {
		return release, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		log.Warn("not registering this run", "error", err)
		return release, nil
	}
	// Twice at most: the run that held the file can remove it between this
	// opening it and locking it, and then what is locked is a file nobody can
	// find. Once more opens the one that is there now.
	for range 2 {
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			log.Warn("not registering this run", "error", err)
			return release, nil
		}
		if err := lock(f); err != nil {
			f.Close()
			if errors.Is(err, errLocked) {
				body, _ := os.ReadFile(path)
				return nil, fmt.Errorf("%w as pid %s, in the background or another terminal: npx %s -k ends it, and npx %s -kd starts it again",
					v1.ErrRunning, strings.TrimSpace(string(body)), v1.CommandName, v1.CommandName)
			}
			log.Warn("not registering this run", "error", err)
			return release, nil
		}
		if !same(f, path) {
			f.Close()
			continue
		}
		pid := strconv.Itoa(os.Getpid()) + "\n"
		if err := f.Truncate(0); err == nil {
			_, err = f.WriteAt([]byte(pid), 0)
		}
		if err != nil {
			log.Warn("not registering this run", "error", err)
			os.Remove(path)
			f.Close()
			return release, nil
		}
		log.Debug("registered this run", "path", path)
		return func() {
			// Removed while still locked where the system allows it, so no
			// other run can have registered in between. Windows will not
			// delete a file this process holds open, so there it goes after
			// the close — and only if it still names this run, since by then
			// another may have taken it.
			if os.Remove(path) != nil {
				f.Close()
				if body, err := os.ReadFile(path); err == nil && string(body) == pid {
					os.Remove(path)
				}
				return
			}
			f.Close()
		}, nil
	}
	log.Warn("not registering this run: its file kept changing underneath it", "path", path)
	return release, nil
}

// same reports whether path still names the file f has open.
func same(f *os.File, path string) bool {
	held, err := f.Stat()
	if err != nil {
		return false
	}
	named, err := os.Stat(path)
	return err == nil && os.SameFile(held, named)
}

// Detach hands a run back from the launcher waiting on it, and reports
// whether there was one. The addresses and the banner have gone to the
// launcher's streams, which are the caller's; from here stdout and stderr go
// to out — the run's log file, which is not this package's to open or close —
// or nowhere when out is nil. They are moved and then the launcher is
// signalled, in that order, so that once it exits nothing here holds a stream
// of the caller's: `$(npx tunneld -d …)` returns, and nothing lands on a
// prompt later.
//
// A run that cannot move its streams still signals: the launcher exits either
// way, and the caller's streams stay held until the run ends, which is logged.
//
// Once only. The launcher exits on the signal, so a second Detach — a run
// whose tunnel was replaced comes up again — has nobody to hand back to, and a
// pid signalled again may belong to another process by then: SIGUSR2 ends one
// that has not asked for it.
func (p *PidImpl) Detach(out *os.File, log v1.Logger) bool {
	if p.parent == 0 {
		return false
	}
	target := out
	if target == nil {
		f, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
		if err != nil {
			log.Warn("cannot let go of the caller's streams", "error", err)
		} else {
			defer f.Close()
			target = f
		}
	}
	if target != nil {
		if err := redirect(target); err != nil {
			log.Warn("cannot let go of the caller's streams", "error", err)
		}
	}
	if err := notify(p.parent); err != nil {
		log.Warn("cannot tell the launcher this run is up", "pid", p.parent, "error", err)
	}
	p.parent = 0
	return true
}
