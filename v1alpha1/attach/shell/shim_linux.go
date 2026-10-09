package shell

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell/ttyshim"
)

// firstSizeWait is how long a run waits for the page's first size before
// starting at 80×24, so the program's first TIOCGWINSZ is already right.
const firstSizeWait = 250 * time.Millisecond

// shimDir is the directory the shim, its terminfo and the pages live in,
// made on the first run: 0700, the shim named by its content, both written
// and read back. The caller holds mu.
func (a *TargetImpl) shimDir() (dir, so string, err error) {
	obj := ttyshim.Object()
	if a.closed {
		return "", "", errors.New("the target is closed")
	}
	if a.dir == "" {
		d, err := shimTemp()
		if err != nil {
			return "", "", err
		}
		if err := writeChecked(filepath.Join(d, shimName(obj)), obj, 0o400); err != nil {
			_ = os.RemoveAll(d)
			return "", "", err
		}
		entry := filepath.Join(d, "terminfo", "x", "xterm-256color")
		if err := os.MkdirAll(filepath.Dir(entry), 0o700); err != nil {
			_ = os.RemoveAll(d)
			return "", "", err
		}
		if err := writeChecked(entry, ttyshim.Terminfo(), 0o400); err != nil {
			_ = os.RemoveAll(d)
			return "", "", err
		}
		a.dir = d
	}
	return a.dir, filepath.Join(a.dir, shimName(obj)), nil
}

// shimTempBases is where shimTemp looks, in order.
var shimTempBases = func() []string { return []string{os.TempDir(), "/tmp", "/dev/shm", "/var/tmp"} }

// shimTemp makes the shim's directory where LD_PRELOAD can name it: its list
// is split on colons and whitespace, so a temp directory holding either is
// passed over for the next one that does not.
func shimTemp() (string, error) {
	last := errors.New("no temp directory LD_PRELOAD can name")
	for _, base := range shimTempBases() {
		// Absolute: a relative LD_PRELOAD is lost once a shell changes
		// directory.
		if abs, err := filepath.Abs(base); err == nil {
			base = abs
		}
		if strings.ContainsAny(base, ": \t\n") {
			continue
		}
		d, err := os.MkdirTemp(base, "tunneld-tty-")
		if err == nil {
			return d, nil
		}
		last = err
	}
	return "", last
}

// writeChecked writes data to path and reads it back: a shim cut short by a
// full disk would load as garbage.
func writeChecked(path string, data []byte, perm os.FileMode) error {
	if err := os.WriteFile(path, data, perm); err != nil {
		return err
	}
	back, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if !bytes.Equal(back, data) {
		return fmt.Errorf("%s reads back different from what was written", path)
	}
	return nil
}

// attachShim is AttachContainer on rung 2: the program on one end of a
// socketpair with the shim preloaded, tunneld on the other, the line
// discipline following the settings the program writes to the page, and
// resize and the signal keys reaching the foreground group it names there.
// Open sets the shim up; should that have gone since, the target turns to
// pipes. A run whose shim never loads carries on with pipes' discipline.
func (a *TargetImpl) attachShim(ctx context.Context, in io.Reader, out, errw io.Writer, resize <-chan remotecommand.TerminalSize) error {
	a.mu.Lock()
	dir, so, err := a.shimDir()
	a.runs++
	pagePath := filepath.Join(dir, fmt.Sprintf("tty-%d", a.runs))
	a.mu.Unlock()
	if err != nil {
		a.mu.Lock()
		closed := a.closed
		a.shim, a.pipes = false, true
		a.mu.Unlock()
		if closed {
			return nil
		}
		a.log.Warn("could not set up the terminal shim; serving this program over pipes from now on", "program", a.ref, "error", err, "cost", pipesNotice)
		return a.attachPipes(ctx, in, out, errw, resize)
	}

	fds, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_STREAM|unix.SOCK_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("run %s: %w", a.ref, err)
	}
	// Ours non-blocking, so Close unblocks a Read parked on it; theirs as a
	// terminal is, blocking.
	if err := unix.SetNonblock(fds[0], true); err != nil {
		_ = unix.Close(fds[0])
		_ = unix.Close(fds[1])
		return fmt.Errorf("run %s: %w", a.ref, err)
	}
	ours, theirs := os.NewFile(uintptr(fds[0]), "tty"), os.NewFile(uintptr(fds[1]), "tty-peer")
	var st unix.Stat_t
	if err := unix.Fstat(fds[1], &st); err != nil {
		_ = ours.Close()
		_ = theirs.Close()
		return fmt.Errorf("run %s: %w", a.ref, err)
	}

	rows, cols := uint16(24), uint16(80)
	select {
	case s, ok := <-resize:
		if ok && s.Width > 0 && s.Height > 0 {
			rows, cols = s.Height, s.Width
		}
	case <-time.After(firstSizeWait):
	}
	pg, err := newPage(pagePath, uint64(st.Dev), st.Ino, rows, cols)
	if err != nil {
		_ = ours.Close()
		_ = theirs.Close()
		return fmt.Errorf("run %s: %w", a.ref, err)
	}
	defer func() { _ = pg.Close() }()

	cmd := exec.CommandContext(ctx, a.path, a.args...)
	cmd.Env = shimEnv(os.Environ(), so, pagePath, dir)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = theirs, theirs, theirs
	ownGroup(cmd)
	err = cmd.Start()
	_ = theirs.Close()
	if err != nil {
		_ = ours.Close()
		return fmt.Errorf("run %s: %w", a.ref, err)
	}
	pg.seed(cmd.Process.Pid)

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		_ = ours.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil
	}
	exited := make(chan struct{})
	a.cmd, a.term, a.exited = cmd, ours, exited
	a.mu.Unlock()
	defer close(exited)
	defer func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		_ = a.stop()
	}()

	// Until the shim has loaded the program sees a socket, not a terminal,
	// and pipes' discipline is the one it can use; from then on, its own.
	// However long loading takes (a cold start, a static wrapper that execs
	// a dynamic program), nothing is given up on a clock: only a program
	// that prints, or ends, without the shim turns the target to pipes.
	mode := func() settings {
		if pg.loaded() {
			return pg.settings()
		}
		return pipesMode
	}
	checkLoad := sync.OnceFunc(func() {
		if !pg.loaded() {
			a.log.Warn("the terminal shim did not load; serving this program over pipes from now on", "program", a.ref, "cost", pipesNotice)
			// The runs to come start as pipes do: -i for a bare shell, and
			// pipes' notice on the page.
			a.mu.Lock()
			a.shim, a.pipes = false, true
			a.mu.Unlock()
		}
	})
	signal := func(sig syscall.Signal) {
		if err := signalGroup(cmd.Process, pg.foreground(), sig); err != nil {
			a.log.Debug("could not signal the program", "program", a.ref, "signal", sig, "error", err)
		}
	}

	// Sizes until this attach ends; see AttachContainer for why the reader
	// is ended and waited for rather than told.
	attached, done := context.WithCancel(ctx)
	stopped := make(chan struct{})
	defer func() {
		done()
		<-stopped
	}()
	go func() {
		defer close(stopped)
		for {
			select {
			case s, ok := <-resize:
				if !ok {
					return
				}
				if s.Width == 0 || s.Height == 0 {
					continue
				}
				pg.setSize(s.Height, s.Width)
				signal(syscall.SIGWINCH)
			case <-attached.Done():
				return
			}
		}
	}()

	settle := func() {}
	if in != nil {
		keys := &cooked{echo: out, stdin: halfCloser{ours}, mode: mode, flushes: pg.flushes, signal: func(k signalKey) {
			signal(keySignals[k])
		}}
		settle = keys.settle
		go func() { _, _ = io.Copy(keys, in) }()
	}

	waited := make(chan struct{})
	go func() {
		if err := cmd.Wait(); err != nil {
			a.log.Debug("program ended", "program", a.ref, "error", err)
		}
		close(waited)
		// A program gone, something it started still holding the socket:
		// bounded as pipes' are.
		select {
		case <-time.After(pipeWait):
			_ = ours.Close()
		case <-attached.Done():
		}
	}()

	first := firstWrite{w: oproc{w: out, mode: mode}, before: checkLoad}
	err = copyShim(&first, ours, pg, settle)
	a.mu.Lock()
	_ = a.stop()
	a.mu.Unlock()
	<-waited
	checkLoad()
	return err
}

// keySignals is what each signal key sends.
var keySignals = [...]syscall.Signal{sigInterrupt: syscall.SIGINT, sigQuit: syscall.SIGQUIT, sigSuspend: syscall.SIGTSTP}

// ackTick is how often the output side wakes with nothing to read: to
// acknowledge a drain the shim is waiting on, and to hand over a line typed
// before the program went raw.
const ackTick = 10 * time.Millisecond

// copyShim copies the program's output to out until the socket ends, as
// CopyOutput does, acknowledging each drain the shim asks for once every
// byte read before it has been through out: the shim changes output
// processing only then, so no byte is processed under settings written
// after it.
func copyShim(out io.Writer, src *os.File, pg *page, settle func()) error {
	buf := make([]byte, 32<<10)
	for {
		g := pg.want()
		_ = src.SetReadDeadline(time.Now().Add(ackTick))
		n, err := src.Read(buf)
		if n > 0 {
			if _, werr := out.Write(buf[:n]); werr != nil {
				return werr
			}
		}
		pg.ack(g)
		settle()
		switch {
		case err == nil, errors.Is(err, os.ErrDeadlineExceeded):
		case errors.Is(err, io.EOF), errors.Is(err, os.ErrClosed), errors.Is(err, syscall.EIO):
			return nil
		default:
			return err
		}
	}
}

// firstWrite runs before once, ahead of the first bytes written through it.
type firstWrite struct {
	w      io.Writer
	before func()
	once   sync.Once
}

func (f *firstWrite) Write(p []byte) (int, error) {
	f.once.Do(f.before)
	return f.w.Write(p)
}

// halfCloser keeps cooked's Ctrl-D on an empty line from closing the socket,
// which carries the program's output too: it shuts down the write half
// instead. A terminal's EOF is one read of nothing; a socket has no such
// thing, so this ends the program's input for the rest of the run, as closing
// stdin does over pipes.
type halfCloser struct{ f *os.File }

func (n halfCloser) Write(p []byte) (int, error) { return n.f.Write(p) }

func (n halfCloser) Close() error {
	raw, err := n.f.SyscallConn()
	if err != nil {
		return err
	}
	var shutErr error
	if err := raw.Control(func(fd uintptr) { shutErr = unix.Shutdown(int(fd), unix.SHUT_WR) }); err != nil {
		return err
	}
	return shutErr
}
