// Package shell serves a local program as an attachable target.
//
// It sits beside attach/docker as the second answer to "this origin is not an
// HTTP service": docker resolves a container reference through the daemon, and
// this package resolves a command through the machine tunneld is running on,
// then runs it on a pseudo-terminal and streams that. The parser asks
// IsExecutable one question while origins are settled — is this word something
// we can run — and rewrites what it says yes to under v1.FileScheme, so a
// program is an origin like any other from there onwards.
//
// It knows nothing about the tunnel or the page: attach.Target is the whole of
// what it provides, which is the same contract docker satisfies. The package
// that hosts those contracts imports neither provider, so the assertions that
// they are satisfied live here and in docker rather than there.
package shell

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sync"

	"github.com/creack/pty"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
)

// The contracts this package satisfies, checked here so a drift fails the
// build rather than the first attach. They are asserted in the provider rather
// than beside the interfaces: attach cannot import this package, since this one
// imports attach for the contract it implements.
var (
	_ attach.Target     = (*TargetImpl)(nil)
	_ attach.Targets    = (*TargetsImpl)(nil)
	_ attach.Repeatable = (*TargetImpl)(nil)
)

// Resolve answers whether s names a program this machine can run, and with
// what: the absolute path of the thing that would actually run.
//
// The path rather than a bare yes, because the origin carries it from here on.
// "top" says which program only on the machine that resolved it, while
// /usr/bin/top says it anywhere — so that is what the frame puts in its corner,
// what the reported map prints, and what somebody pastes back into a command
// line to get the same program rather than whatever their own $PATH finds.
//
// exec.LookPath is the whole of the lookup, which means the rules are the
// host's rather than ours: the $PATH search and the executable bit on Unix,
// the PATHEXT extensions on Windows, and a value carrying a separator taken as
// a path to the file itself rather than as a name to search for. Anything
// shaped like a URL answers false for that last reason — "http://localhost:3000"
// is stat'd as a path and is not there.
//
// A command that resolves only through the working directory answers false
// too: LookPath returns exec.ErrDot for it, and a file that happens to sit
// where tunneld was started should not become a public origin because somebody
// typed its name.
//
// The answer is about this machine and this moment. The same argument is a
// program on one host and a hostname on another, which is why nothing here is
// cached and why the lookup happens while origins are settled rather than when
// they are seeded.
func Resolve(s string) (string, bool) {
	path, err := exec.LookPath(s)
	if err != nil {
		return "", false
	}
	// LookPath answers a PATH hit absolutely and a path-shaped argument as it
	// was given, so this is what makes the two agree.
	//
	// On Windows the absolute form is C:\..., which has no spelling inside a
	// file:// URL — whichever half of the URL it is put in, url.URL escapes
	// the separators. The origin is still correct where it counts, since the
	// binder reads the path off the URL rather than off its printed form, and
	// the platform has no pseudo-terminals to serve it on either way.
	abs, err := filepath.Abs(path)
	if err != nil {
		return path, true
	}
	return abs, true
}

// Option configures a TargetsImpl at construction. There are none yet; the
// signature exists so a knob added later changes no caller.
type Option = v1.Option[*TargetsImpl]

// TargetsImpl is the default source of targets: programs on this machine's
// $PATH, run on a pseudo-terminal.
type TargetsImpl struct{}

// New returns the default source of targets, configured by opts.
func New(opts ...Option) *TargetsImpl {
	return v1.Apply(&TargetsImpl{}, opts...)
}

// Scheme is v1.FileScheme: this provider answers file:// origins and no
// others, which is the whole of how the binder picks it.
func (*TargetsImpl) Scheme() string { return v1.FileScheme }

// Open resolves ref — a command name, or a path to an executable — against
// this machine, and checks that the machine can give it a terminal.
//
// Nothing is run here. What the resolution buys is the same thing docker's
// inspect buys: everything that can fail about the origin fails before the
// tunnel is minted, rather than as a page that answers an error to whoever was
// handed the URL. The two failures have different levers, so they are worded
// apart — a name that resolves to nothing is the origin's fault (fix it, or
// install the program), and a platform with no pseudo-terminals is not.
//
// The pty probe is a real one: it opens a pair and closes it again, because
// there is nothing to ask short of trying. On Windows it is what turns
// "every visit fails" into "this run refuses to start".
func (*TargetsImpl) Open(_ context.Context, ref string, log v1.Logger) (attach.Target, error) {
	path, err := exec.LookPath(ref)
	if err != nil {
		return nil, fmt.Errorf("%w: %q names no program this machine can run: %w", v1.ErrInvalidOrigin, ref, err)
	}

	master, slave, err := pty.Open()
	if err != nil {
		return nil, fmt.Errorf("cannot give %q a terminal on this platform: %w", ref, err)
	}
	_ = slave.Close()
	_ = master.Close()

	log.Debug("resolved a program as an origin", "program", ref, "path", path)
	return &TargetImpl{ref: ref, path: path, log: log}, nil
}

// TargetImpl is one program, resolved but not yet running. The process and its
// terminal are created by AttachContainer and torn down by Close, so a target
// nobody attaches to costs nothing.
type TargetImpl struct {
	ref  string
	path string
	log  *slog.Logger

	// mu guards the running process and its terminal, which exist only
	// between an attach starting and either end of it finishing. Close can
	// arrive from a different goroutine at any point in that window — a
	// shutdown, or a caller's defer — which is the whole reason for the lock.
	mu     sync.Mutex
	cmd    *exec.Cmd
	term   *os.File
	closed bool
}

// Name is the reference the operator typed, not the resolved path: it is what
// they will recognize in a page title and a log line.
func (a *TargetImpl) Name() string { return a.ref }

// Scheme is v1.FileScheme, which with Name reconstructs the origin exactly as
// it was typed — including the bare word the parser rewrote into one.
func (a *TargetImpl) Scheme() string { return v1.FileScheme }

// TTY is always true. A program run here is given a pseudo-terminal whether or
// not it would have had one, because the point of the origin is the terminal:
// a full-screen program needs one to draw at all, and a line-oriented one is
// no worse for having it.
func (a *TargetImpl) TTY() bool { return true }

// Stdin is always true, for the same reason TTY is: the page is a terminal,
// and a terminal that cannot be typed into is a log viewer.
func (a *TargetImpl) Stdin() bool { return true }

// Repeatable is true while the program is still there to run. Each attach
// starts it — AttachContainer builds the command itself — so a second one is a
// second run rather than a resumed one, which is what lets an origin outlive
// the program that was serving it.
//
// The path is checked again rather than remembered: a program uninstalled
// since the origin was resolved cannot be run, and answering yes would trade a
// terminal that says the program ended for one that fails to start it.
func (a *TargetImpl) Repeatable() bool {
	_, err := os.Stat(a.path)
	return err == nil
}

// Close ends the program and releases its terminal. It tolerates a second
// call, which it has to: Server.Close calls it unconditionally from both the
// context.AfterFunc registered in Serve and a caller's own defer.
//
// Closing the terminal is what unblocks a copy parked reading it, so this is
// also how an attach on a program that prints nothing is brought down.
func (a *TargetImpl) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.closed = true
	return a.stop()
}

// stop kills whatever is running and closes its terminal, leaving the target
// with nothing held. The caller holds mu.
//
// The process is killed rather than signalled politely: it was started for one
// viewer's terminal and there is nobody left to read what it might say on the
// way out. An already-finished process is not an error, which is the common
// case — the program exited, the copy ended, and this is the cleanup.
func (a *TargetImpl) stop() error {
	if a.term != nil {
		_ = a.term.Close()
		a.term = nil
	}
	if a.cmd != nil && a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
	}
	a.cmd = nil
	return nil
}

// AttachContainer runs the program on a pseudo-terminal and copies until it
// exits, the viewer leaves, or ctx is canceled. The Kubernetes-shaped name,
// uid and container arguments are ignored: this TargetImpl is one program by
// construction.
//
// There is no backlog to replay, which is the one place this departs from the
// container provider: a container has been running and printing since long
// before anybody opened the page, while a program starts here, when the attach
// does. What the terminal shows is everything it has ever printed.
//
// errw is unused. A pseudo-terminal is one stream by construction — the child's
// stdout and stderr are the same file — so there is nothing to demultiplex and
// nothing to put on a channel of its own.
func (a *TargetImpl) AttachContainer(ctx context.Context, _, _, _ string, in io.Reader, out, errw io.WriteCloser, tty bool, resize <-chan remotecommand.TerminalSize) error {
	cmd := exec.CommandContext(ctx, a.path)
	// TERM is what makes a full-screen program willing to draw. The value is
	// the one the page's terminal emulator implements, and the same one the
	// frame announces to a container.
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")

	term, err := pty.Start(cmd)
	if err != nil {
		return fmt.Errorf("run %s: %w", a.ref, err)
	}
	// Software flow control off before the program can write a byte, so
	// nobody's Ctrl-S freezes the screen for everybody else. Not fatal if it
	// fails: what is lost is a key behaving oddly, where refusing to run the
	// program at all would lose the origin.
	if err := unmeter(term); err != nil {
		a.log.Debug("could not turn off flow control", "program", a.ref, "error", err)
	}

	// Published under the lock so Close can reach them, and refused outright
	// if Close already happened: a target closed while the program was
	// starting must not leave one behind that nothing will ever kill.
	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		_ = term.Close()
		_ = cmd.Process.Kill()
		return nil
	}
	a.cmd, a.term = cmd, term
	a.mu.Unlock()

	defer func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		_ = a.stop()
	}()

	// Sizes arrive until the channel closes, which ServeAttach does when the
	// socket ends. A zero is the page saying it does not know yet; forwarding
	// it would tell the program it has no room at all.
	go func() {
		for size := range resize {
			if size.Width == 0 || size.Height == 0 {
				continue
			}
			if err := pty.Setsize(term, &pty.Winsize{Rows: size.Height, Cols: size.Width}); err != nil && ctx.Err() == nil {
				a.log.Debug("could not resize program", "program", a.ref, "error", err)
			}
		}
	}()

	if in != nil {
		go func() {
			// Bounded from both ends: a write parked on the terminal dies when
			// the deferred stop closes it, and a read parked on in ends when
			// the caller closes it — which ServeAttach does the moment this
			// function returns, since tearing the websocket down closes every
			// channel on it.
			_, _ = io.Copy(term, in)
		}()
	}

	// The same read every provider does, in the same place, told the same way
	// which shape to expect: tty is ServeAttach's copy of TTY, which is true
	// here by construction, so this takes the raw branch and errw stays
	// untouched. What the format is, and what counts as the stream simply
	// ending, are decided there rather than here — Linux reporting the last
	// writer's exit as EIO where the BSDs report EOF is exactly the kind of
	// difference that belongs in one place.
	err = attach.CopyOutput(out, errw, term, tty)

	// The program's own exit status is not tunneld's failure — a program that
	// ends with an error has still done exactly what it was asked to — so it
	// is reported where somebody debugging would look and nowhere else.
	if wait := cmd.Wait(); wait != nil {
		a.log.Debug("program ended", "program", a.ref, "error", wait)
	}
	return err
}
