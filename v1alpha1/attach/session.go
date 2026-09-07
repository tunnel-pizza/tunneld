package attach

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"

	"github.com/charmbracelet/x/vt"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// The screen the emulator keeps before any viewer has said how big its window
// is. It is only ever the size of the first repaint, because the first viewer
// to connect resizes everything to its own window.
const (
	defaultCols = 80
	defaultRows = 24
)

// How much scrolled-off output a viewer is given on connect. Lines rather than
// bytes because that is the emulator's own bound, and a thousand of them is
// both more than a person scrolls back through and small enough that a
// container talking for an hour cannot grow tunneld without limit.
const scrollbackLines = 1000

// How many writes a viewer may fall behind before it is dropped. A viewer that
// cannot keep up is a dead socket that has not admitted it yet, and waiting on
// one would stall the container's output for everyone else. Dropping it is
// safe because reconnecting is cheap: the next connection is handed the whole
// screen back.
const viewerBacklog = 64

// session is one attach to a target, shared by every viewer of it.
//
// The alternative, and what this replaces, was an attach per viewer: every
// page load opened its own stream to the target and asked it to replay
// whatever backlog it had. That reproduced the bytes but not the screen they
// had produced, so the app inside and the browser's terminal disagreed about
// where the cursor was, and the app's next repaint landed at the wrong origin
// and drew over the restored screen.
//
// Here the stream is opened once and never restarted. Everything it writes
// goes through an emulator, which is the thing that actually knows what the
// screen looks like, so a viewer arriving late is handed the screen rather
// than the bytes that once produced it. The app is never told, because from
// its side nothing happened.
//
// It embeds Target so Name, TTY, Stdin and Close pass through, and overrides
// AttachContainer, which is how ServeAttach reaches a viewer join instead of
// the target itself.
type session struct {
	Target

	log *slog.Logger

	// stdin is the write end of the pipe feeding the target. Every viewer's
	// keystrokes go here, interleaved, which is what sharing one terminal
	// means.
	stdin *io.PipeWriter
	// resize carries the negotiated size to the target. Unbuffered, and only
	// ever sent to from a goroutine, so a target that is slow to read one
	// cannot hold the lock.
	resize chan remotecommand.TerminalSize
	done   chan struct{}

	// em is the screen. It does its own locking, so it is deliberately not
	// under mu: writing to it can block until its replies are drained, and a
	// viewer joining must never wait on that.
	em *vt.SafeEmulator

	// mu guards the viewer set and the size negotiated from it, and nothing
	// else.
	mu      sync.Mutex
	viewers map[*viewer]struct{}
	size    remotecommand.TerminalSize
}

// viewer is one connected browser. Output reaches it through a channel rather
// than a direct write so that one slow socket cannot stall the emulator, and
// through it every other viewer.
type viewer struct {
	out  chan []byte
	size remotecommand.TerminalSize
}

// newSession opens the one attach and starts feeding the emulator from it. It
// returns as soon as the stream is running; a target that fails is reported
// through the log, because by this point the tunnel is already up and a dead
// terminal origin is not worth taking it down.
func newSession(ctx context.Context, target Target, log *slog.Logger) *session {
	pr, pw := io.Pipe()
	em := vt.NewSafeEmulator(defaultCols, defaultRows)
	em.SetScrollbackSize(scrollbackLines)

	s := &session{
		Target:  target,
		log:     log,
		stdin:   pw,
		resize:  make(chan remotecommand.TerminalSize),
		done:    make(chan struct{}),
		em:      em,
		viewers: map[*viewer]struct{}{},
		size:    remotecommand.TerminalSize{Width: defaultCols, Height: defaultRows},
	}

	// The emulator answers what a real terminal answers — a device-attributes
	// query, a cursor-position report — and those replies have to reach the
	// app or they pile up until the emulator stops accepting output at all.
	// That is a deadlock, not a slow path: the write that fills the buffer
	// never returns, so nothing is ever drawn again. Copying them into the
	// same stdin the viewers type on is what a terminal does with them.
	go func() {
		if _, err := io.Copy(pw, em); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			log.Debug("terminal replies stopped", "container", target.Name(), "error", err)
		}
	}()

	go func() {
		defer close(s.done)
		defer func() { _ = pr.Close() }()

		name := target.Name()
		out := &sink{s: s}
		if err := target.AttachContainer(ctx, name, "", name, pr, out, out, target.TTY(), s.resize); err != nil &&
			ctx.Err() == nil {
			log.Debug("attach session ended", "container", name, "error", err)
		}
	}()
	return s
}

// close ends the shared attach. The pipe is what the target is reading, so
// closing it is what unblocks it.
func (s *session) close() error {
	return s.stdin.Close()
}

// sink is where the target's output lands: into the emulator, which is the
// screen, and out to whoever is watching.
type sink struct{ s *session }

func (w *sink) Close() error { return nil }

func (w *sink) Write(p []byte) (int, error) {
	s := w.s

	// The screen first, then the people watching it. A viewer that joins
	// between the two is handed a screen that already has these bytes and
	// then receives them again, which repaints the same cells; one that joins
	// the other way round would miss them entirely, which does not.
	_, _ = s.em.Write(p)

	s.mu.Lock()
	for v := range s.viewers {
		// A copy per viewer: p belongs to the caller and is reused.
		frame := make([]byte, len(p))
		copy(frame, p)
		select {
		case v.out <- frame:
		default:
			// Backlogged. Drop the viewer rather than the byte, and let it
			// come back to a fresh screen.
			delete(s.viewers, v)
			close(v.out)
		}
	}
	s.mu.Unlock()
	return len(p), nil
}

// AttachContainer joins a viewer to the shared session, which is what
// ServeAttach calls once per websocket.
//
// It does not reach the target at all. The viewer is handed the screen as it
// stands and then the live stream, and its window size is applied whenever it
// arrives rather than waited for: a client that never sends one still gets a
// working terminal at whatever size the session already settled on, and the
// page's own size lands a moment later and repaints. Blocking on it here
// would hang any client that does not send one, which the streaming protocol
// does not require.
//
// The resize channel closing is how the socket says it is gone. ServeAttach
// opens that stream unconditionally and closes it when the connection ends,
// which makes it the one signal that works whether the page closed cleanly or
// the laptop lid did.
func (s *session) AttachContainer(ctx context.Context, _, _, _ string, in io.Reader, out, _ io.WriteCloser, _ bool, resize <-chan remotecommand.TerminalSize) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	v := &viewer{out: make(chan []byte, viewerBacklog)}

	negotiated := s.join(v)
	defer s.part(v)
	s.apply(ctx, negotiated)

	go s.pump(ctx, cancel, in)
	go s.follow(ctx, cancel, v, resize)

	// Writing happens here rather than in follow so that a write that blocks
	// is this viewer's problem and nobody else's.
	for {
		select {
		case frame, ok := <-v.out:
			if !ok {
				return nil // dropped for falling behind
			}
			if _, err := out.Write(frame); err != nil {
				return nil
			}
		case <-ctx.Done():
			return nil
		case <-s.done:
			return nil
		}
	}
}

// join registers a viewer and seeds its queue with the screen as it stands,
// returning the size the session settled on. Both happen under one lock so the
// screen a viewer is given and the stream it then receives cannot overlap or
// leave a gap.
func (s *session) join(v *viewer) remotecommand.TerminalSize {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.viewers[v] = struct{}{}
	negotiated := s.negotiate()
	v.out <- repaint(s.em)
	return negotiated
}

// part removes a viewer and renegotiates, since the smallest window may have
// been the one that left.
func (s *session) part(v *viewer) {
	s.mu.Lock()
	_, present := s.viewers[v]
	delete(s.viewers, v)
	negotiated := s.negotiate()
	s.mu.Unlock()

	if present {
		s.apply(context.Background(), negotiated)
	}
}

// negotiate settles the pty on the smallest window watching it, and resizes
// the emulator to match. Smallest rather than newest or first, because the pty
// has one size and every viewer renders all of it: anything larger than the
// smallest window is drawn wrapped or clipped there, which looks exactly like
// the corruption this whole session exists to prevent. A larger window gets
// unused margin instead, which is merely wasteful.
//
// The caller holds the lock, and applies the result after releasing it.
func (s *session) negotiate() remotecommand.TerminalSize {
	var w, h uint16
	for v := range s.viewers {
		if v.size.Width == 0 || v.size.Height == 0 {
			continue
		}
		if w == 0 || v.size.Width < w {
			w = v.size.Width
		}
		if h == 0 || v.size.Height < h {
			h = v.size.Height
		}
	}
	if w == 0 || h == 0 || (w == s.size.Width && h == s.size.Height) {
		return remotecommand.TerminalSize{} // nothing to apply
	}
	s.size = remotecommand.TerminalSize{Width: w, Height: h}
	s.em.Resize(int(w), int(h))
	return s.size
}

// apply forwards a settled size to the target, if there was one. Off the lock:
// the target reads this channel from the goroutine that is also writing output
// back through sink, which takes the lock.
func (s *session) apply(ctx context.Context, size remotecommand.TerminalSize) {
	if size.Width == 0 || size.Height == 0 {
		return
	}
	select {
	case s.resize <- size:
	case <-ctx.Done():
	case <-s.done:
	}
}

// pump copies one viewer's keystrokes into the shared stdin.
func (s *session) pump(ctx context.Context, cancel context.CancelFunc, in io.Reader) {
	defer cancel()
	buf := make([]byte, 4096)
	for {
		n, err := in.Read(buf)
		if n > 0 {
			// No lock: io.Pipe serialises its writers, and taking ours here
			// would hold it for as long as the app takes to read.
			if _, werr := s.stdin.Write(buf[:n]); werr != nil {
				return
			}
		}
		if err != nil || ctx.Err() != nil {
			return
		}
	}
}

// follow tracks one viewer's window size, and treats the channel closing as
// the socket having gone.
func (s *session) follow(ctx context.Context, cancel context.CancelFunc, v *viewer, resize <-chan remotecommand.TerminalSize) {
	defer cancel()
	for {
		select {
		case size, ok := <-resize:
			if !ok {
				return
			}
			s.mu.Lock()
			v.size = size
			negotiated := s.negotiate()
			s.mu.Unlock()
			s.apply(ctx, negotiated)
		case <-ctx.Done():
			return
		case <-s.done:
			return
		}
	}
}

// repaint is the screen as it stands, as the bytes that reproduce it: what
// scrolled off, then the visible screen, then the cursor put back where the
// app believes it is.
//
// That last part is the whole point. A byte replay leaves the cursor wherever
// the replayed stream happened to end, which is not where the app thinks it
// is, and the app's next differential redraw is measured from its own belief —
// so it lands at the wrong origin and paints over what is already there.
func repaint(em *vt.SafeEmulator) []byte {
	var b bytes.Buffer

	// Reset attributes, clear the screen and the browser's own scrollback, and
	// go home, so nothing from a previous connection is underneath this.
	b.WriteString("\x1b[0m\x1b[2J\x1b[3J\x1b[H")

	// An app on the alternate screen has no scrollback to restore, and the
	// viewer's terminal has to be put on the alternate screen too or the
	// repaint lands on the wrong buffer and is still there after the app
	// exits.
	if em.IsAltScreen() {
		b.WriteString("\x1b[?1049h")
	} else {
		for _, line := range em.Scrollback().Lines() {
			b.WriteString(line.Render())
			b.WriteString("\r\n")
		}
	}

	// Render separates lines with \n. The page sets convertEol, so that alone
	// would do, but a repaint that depends on a terminal option set elsewhere
	// is one rename away from drawing a staircase.
	b.WriteString(strings.ReplaceAll(em.Render(), "\n", "\r\n"))

	pos := em.CursorPosition()
	fmt.Fprintf(&b, "\x1b[%d;%dH", pos.Y+1, pos.X+1)
	return b.Bytes()
}
