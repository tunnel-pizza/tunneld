package shell

import (
	"bytes"
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// A machine with no pseudo-terminals — a sandbox with a minimal /dev, or
// Windows — still runs programs. What it cannot give them is a terminal, so a
// target there is served over pipes instead: the program's stdin, stdout and
// stderr, with tunneld standing in for the line discipline a terminal would
// have had (cooked, below). Line-oriented programs work; full-screen ones have
// nothing to draw on, and nothing can be resized.

// pipesNotice is what the page says about a program served over pipes, where a
// container's page names the docker flag it was started without.
const pipesNotice = "no terminal on this machine — no line editing, no resize, no full-screen programs"

// shells are programs that read a terminal-less stdin as a script unless told
// otherwise. Run with no arguments over pipes, each is given -i: it prompts,
// and it takes the interrupt meant for what it is running as the prompt
// coming back rather than dying of it, which is what it would have done on a
// terminal.
var shells = []string{"ash", "bash", "dash", "ksh", "mksh", "sh", "zsh"}

// pipeArgs is what a program is run with over pipes: its own arguments, or -i
// for a shell that was given none.
//
// bash also gets --noediting. With -i it runs readline even on a pipe, and
// readline echoes each line it reads — a second echo after cooked's — and
// answers Tab by redrawing a line it has no terminal to redraw on. Asked of
// the program the path resolves to, since sh is bash on many systems; and
// only of bash, since dash and ash refuse the option.
func pipeArgs(path string, args []string) []string {
	if len(args) > 0 || !slices.Contains(shells, baseName(path)) {
		return args
	}
	resolved := path
	if r, err := filepath.EvalSymlinks(path); err == nil {
		resolved = r
	}
	if baseName(path) == "bash" || baseName(resolved) == "bash" {
		return []string{"--noediting", "-i"}
	}
	return []string{"-i"}
}

// baseName is a program's name without its directory or extension, read off
// either separator, since the path is the host's.
func baseName(path string) string {
	name := path[strings.LastIndexAny(path, `/\`)+1:]
	return strings.TrimSuffix(name, filepath.Ext(name))
}

// oproc is a terminal's output processing: while OPOST and ONLCR are both set,
// every newline becomes a carriage return and a newline. Without it a line
// feed only moves down, and the screen the frame draws from starts each line
// where the last one ended. A program that clears OPOST (vi) writes its own
// CR LF, and must not get a second CR. mode nil is pipesMode.
type oproc struct {
	w    io.Writer
	mode func() settings
}

func (o oproc) Write(p []byte) (int, error) {
	m := pipesMode
	if o.mode != nil {
		m = o.mode()
	}
	if m.oflag&oOPOST == 0 || m.oflag&oONLCR == 0 || bytes.IndexByte(p, '\n') < 0 {
		return o.w.Write(p)
	}
	if _, err := o.w.Write(bytes.ReplaceAll(p, []byte("\n"), []byte("\r\n"))); err != nil {
		return 0, err
	}
	return len(p), nil
}

// pipeWait bounds how long an ended program's output is waited for. A program
// that started something in the background and left it holding stdout would
// otherwise hold the attach open for as long as that runs.
const pipeWait = 2 * time.Second

// attachPipes is AttachContainer for a machine with no pseudo-terminals: the
// program on pipes, its output copied as it comes, and keystrokes put through
// cooked on their way in. It returns when the program exits or ctx ends.
func (a *TargetImpl) attachPipes(ctx context.Context, in io.Reader, out, errw io.Writer, resize <-chan remotecommand.TerminalSize) error {
	cmd := exec.CommandContext(ctx, a.path, pipeArgs(a.path, a.args)...)
	// dumb says what the output is going to: no cursor to move, no colors
	// anybody asked for.
	cmd.Env = append(os.Environ(), "TERM=dumb")
	// Through oproc, the one piece of a terminal's output processing a
	// program counts on. out and errw are one writer when the caller passes
	// one, and stay one — exec gives both streams a single pipe when they
	// compare equal, so what the program interleaves stays interleaved.
	cmd.Stdout, cmd.Stderr = oproc{w: out}, oproc{w: errw}
	cmd.WaitDelay = pipeWait
	ownGroup(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	a.mu.Lock()
	if a.closed {
		a.mu.Unlock()
		_ = stdin.Close()
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return nil
	}
	exited := make(chan struct{})
	a.cmd, a.term, a.exited = cmd, stdin, exited
	a.mu.Unlock()
	defer close(exited)
	defer func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		_ = a.stop()
	}()

	// Sizes still arrive, and a sender waits for them to be taken, so they
	// are taken — and dropped, with nothing to give them to. Until this
	// attach ends, for the reason AttachContainer's reader stops then.
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
			case _, ok := <-resize:
				if !ok {
					return
				}
			case <-attached.Done():
				return
			}
		}
	}()

	if in == nil {
		_ = stdin.Close()
	} else {
		keys := &cooked{echo: out, stdin: stdin, signal: func(k signalKey) {
			if k != sigInterrupt {
				return
			}
			if err := interrupt(cmd.Process); err != nil {
				a.log.Debug("could not interrupt the program", "program", a.ref, "error", err)
			}
		}}
		go func() { _, _ = io.Copy(keys, in) }()
	}

	if wait := cmd.Wait(); wait != nil {
		a.log.Debug("program ended", "program", a.ref, "error", wait)
	}
	return nil
}

// cooked is the line discipline a terminal would have given the program,
// following the settings it last set (mode; nil is pipesMode). In canonical
// mode keystrokes are echoed and gathered into lines, a line sent when Enter
// ends it, and the keys that edit or signal handled here. In raw mode (vi,
// readline) each byte goes to the program as it arrives, escape sequences
// included. What a person types at the page arrives as a terminal's keys —
// Enter is \r, Backspace is DEL.
//
// In canonical mode, arrow keys and other escape sequences are dropped: there
// is no line editing to move through, and passed on they would be typed into
// the line as noise.
type cooked struct {
	echo   io.Writer
	stdin  io.WriteCloser
	mode   func() settings
	signal func(signalKey)
	// flushes counts the input flushes the program asked for; each takes the
	// line being edited with it. nil: none are ever asked for.
	flushes func() uint32

	// mu serialises keys against settle, which the output side calls.
	mu      sync.Mutex
	flushed uint32

	line []byte
	// esc is how far into an escape sequence the input is: 0 outside one, 1
	// after ESC, 2 inside a CSI or SS3 sequence, which runs to a final byte.
	esc int
	// cr remembers a \r just ended a line, so a \n straight after it — a
	// pasted CRLF — is not a second, empty one.
	cr bool
}

func (c *cooked) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropFlushed()
	m := pipesMode
	if c.mode != nil {
		m = c.mode()
	}
	if m.lflag&lICANON == 0 {
		return c.raw(p, m)
	}
	for _, b := range p {
		if err := c.key(b, m); err != nil {
			return 0, err
		}
	}
	return len(p), nil
}

// settle hands a line typed without Enter to a program that has since gone
// raw, as a terminal does the moment ICANON goes, without waiting for a
// further key to carry it; and drops one an input flush took.
func (c *cooked) settle() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.dropFlushed()
	if len(c.line) == 0 || c.mode == nil || c.mode().lflag&lICANON != 0 {
		return
	}
	_, _ = c.stdin.Write(c.line)
	c.line, c.esc, c.cr = c.line[:0], 0, false
}

// dropFlushed forgets the line being edited if the program has asked for an
// input flush since. The caller holds mu.
func (c *cooked) dropFlushed() {
	if c.flushes == nil {
		return
	}
	if f := c.flushes(); f != c.flushed {
		c.flushed = f
		c.line, c.esc, c.cr = c.line[:0], 0, false
	}
}

// raw hands p to the program as it is, but for the keys ISIG makes signals
// and the CR that ICRNL makes a newline.
func (c *cooked) raw(p []byte, m settings) (int, error) {
	// A line typed without Enter before the switch is the program's to read
	// now, as a terminal hands its line buffer over when ICANON goes. It was
	// echoed as it was typed.
	if len(c.line) > 0 {
		if _, err := c.stdin.Write(c.line); err != nil {
			return 0, err
		}
	}
	c.line, c.esc, c.cr = c.line[:0], 0, false
	out := make([]byte, 0, len(p))
	flush := func() error {
		if len(out) == 0 {
			return nil
		}
		if m.lflag&lECHO != 0 {
			// Echo goes out as output does: a newline gets its CR under
			// OPOST and ONLCR.
			shown := out
			if m.oflag&oOPOST != 0 && m.oflag&oONLCR != 0 {
				shown = bytes.ReplaceAll(out, []byte("\n"), []byte("\r\n"))
			}
			c.say(string(shown))
		}
		_, err := c.stdin.Write(out)
		out = out[:0]
		return err
	}
	for _, b := range p {
		if m.lflag&lISIG != 0 {
			if k, ok := m.signalFor(b); ok {
				if err := flush(); err != nil {
					return 0, err
				}
				c.signal(k)
				continue
			}
		}
		if b == '\r' && m.iflag&iICRNL != 0 {
			b = '\n'
		}
		out = append(out, b)
	}
	if err := flush(); err != nil {
		return 0, err
	}
	return len(p), nil
}

func (c *cooked) key(b byte, m settings) error {
	switch c.esc {
	case 1:
		c.esc = 0
		if b == '[' || b == 'O' {
			c.esc = 2
		}
		return nil
	case 2:
		if b >= 0x40 && b <= 0x7e {
			c.esc = 0
		}
		return nil
	}
	cr := c.cr
	c.cr = false
	if m.lflag&lISIG != 0 {
		if k, ok := m.signalFor(b); ok {
			c.echoed(m, caret(b)+"\r\n")
			c.line = c.line[:0]
			c.signal(k)
			return nil
		}
	}
	switch {
	case b == 0x1b:
		c.esc = 1
	case b == '\r' || b == '\n':
		if b == '\n' && cr {
			return nil
		}
		c.cr = b == '\r'
		c.echoed(m, "\r\n")
		return c.send(true)
	case b == 0x08 || (m.cc[vERASE] != 0 && b == m.cc[vERASE]):
		c.erase(m, 1)
	case m.cc[vKILL] != 0 && b == m.cc[vKILL]:
		c.erase(m, utf8.RuneCount(c.line))
	case m.cc[vWERASE] != 0 && b == m.cc[vWERASE]:
		c.erase(m, wordRunes(c.line))
	case m.cc[vEOF] != 0 && b == m.cc[vEOF]:
		if len(c.line) == 0 {
			return c.stdin.Close()
		}
		return c.send(false)
	default:
		if b < 0x20 && b != '\t' {
			return nil
		}
		c.line = append(c.line, b)
		c.echoed(m, string([]byte{b}))
	}
	return nil
}

// send hands the line to the program, ended with a newline when Enter ended
// it.
func (c *cooked) send(newline bool) error {
	line := c.line
	if newline {
		line = append(line, '\n')
	}
	c.line = c.line[:0]
	_, err := c.stdin.Write(line)
	return err
}

// erase takes n characters off the end of the line, and off the screen.
func (c *cooked) erase(m settings, n int) {
	for ; n > 0 && len(c.line) > 0; n-- {
		_, size := utf8.DecodeLastRune(c.line)
		c.line = c.line[:len(c.line)-size]
		c.echoed(m, "\b \b")
	}
}

// echoed shows s while ECHO is set.
func (c *cooked) echoed(m settings, s string) {
	if m.lflag&lECHO != 0 {
		c.say(s)
	}
}

func (c *cooked) say(s string) { _, _ = io.WriteString(c.echo, s) }

// wordRunes is how many runes ^W takes off line: trailing spaces, then the
// word before them.
func wordRunes(line []byte) int {
	s := []rune(string(line))
	n := 0
	for n < len(s) && s[len(s)-1-n] == ' ' {
		n++
	}
	for n < len(s) && s[len(s)-1-n] != ' ' {
		n++
	}
	return n
}

// caret is how a terminal echoes a control key: ^C, and ^? for DEL.
func caret(b byte) string {
	if b == 0x7f {
		return "^?"
	}
	return "^" + string(rune(b+0x40))
}
