package attach

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"

	"image/color"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// The screen the emulator keeps before any viewer has said how big its window
// is. It is only ever the size of the first frame, because the first viewer to
// connect resizes everything to its own window.
const (
	defaultCols = 80
	defaultRows = 24
)

// How much scrolled-off output the emulator keeps. Lines rather than bytes
// because that is the emulator's own bound, and a thousand of them is both
// more than a person scrolls back through and small enough that a container
// talking for an hour cannot grow tunneld without limit.
//
// A framed viewer reaches them through the frame's own scroll rather than the
// browser's, which the alternate screen takes away.
const scrollbackLines = 1000

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
// screen looks like, so a viewer arriving late renders the screen rather than
// the bytes that once produced it. The app is never told, because from its
// side nothing happened.
//
// What a viewer's socket carries is not the container's output but a frame
// drawn around it — see frame.go — one frame per viewer, each rendering this one
// emulator. That is what keeps a keystroke meant for the frame out of the
// container, and it is why the emulator is read here rather than copied: a
// frame asks what the screen is, and draws it.
//
// It embeds Target so Name, TTY, Stdin and Close pass through, and overrides
// AttachContainer, which is how ServeAttach reaches a viewer join instead of
// the target itself.
type session struct {
	Target

	log *slog.Logger

	// banner is the build line the frame shows along the bottom. Fixed for the
	// life of the process, so it is read without the lock.
	banner string

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

	// titleMu guards title and nothing else. Deliberately not mu — see
	// newSession, where the callback that writes it is installed.
	titleMu sync.Mutex
	title   string

	// mu guards the viewer set, the size negotiated from it, and the public
	// address, and nothing else.
	mu      sync.Mutex
	viewers map[*viewer]struct{}
	size    remotecommand.TerminalSize
	public  string
}

// viewer is one connected browser: the frame drawing for it, and the window
// that frame is drawing into.
//
// wake is how the container's output reaches that frame. One slot, and a write
// that finds it full drops rather than waits — a redraw carries nothing, so
// two pending ones say exactly what one says, and the alternative is a viewer
// that has stopped reading holding up the container's output for everybody
// else.
type viewer struct {
	prog *tea.Program
	wake chan struct{}
	size remotecommand.TerminalSize
}

// newSession opens the one attach and starts feeding the emulator from it. It
// returns as soon as the stream is running; a target that fails is reported
// through the log, because by this point the tunnel is already up and a dead
// terminal origin is not worth taking it down.
func newSession(ctx context.Context, target Target, banner string, log *slog.Logger) *session {
	pr, pw := io.Pipe()
	em := vt.NewSafeEmulator(defaultCols, defaultRows)
	em.SetScrollbackSize(scrollbackLines)

	s := &session{
		Target:  target,
		log:     log,
		banner:  banner,
		stdin:   pw,
		resize:  make(chan remotecommand.TerminalSize),
		done:    make(chan struct{}),
		em:      em,
		viewers: map[*viewer]struct{}{},
		size:    remotecommand.TerminalSize{Width: defaultCols, Height: defaultRows},
	}

	// Everything the terminal says about itself, in the debug log, and the one
	// thing the frame acts on.
	//
	// These are the channels an app has for talking about its state rather
	// than painting its screen — a title, a working directory, a mode it wants
	// turned on — and most of them tunneld has no use for. They are logged
	// because the only way to find out what a given app actually sends is to
	// watch one send it, and because an app that misbehaves in a frame usually
	// does it here.
	//
	// What the frame shows is the tab title. Oh My Zsh and friends set both
	// titles from preexec and reset them from precmd, so they carry the
	// running command while one runs and the prompt's idea of where it is when
	// none does. The tab title — OSC 1 — is the same fact said shorter: the
	// window title is the whole command line and, at rest, user@host:~, which
	// in a frame that already names the host and the origin is mostly things
	// said twice. A shell that sets neither leaves the frame with nothing to
	// show, which is most of them without a prompt framework.
	//
	// CursorPosition is deliberately not among them: it fires on every cursor
	// move, which is every keystroke and every redraw, and it would drown
	// everything else in the log.
	//
	// All of these run from inside the emulator's write, which is to say with
	// the emulator's lock held. So they may only stash a value or write a line
	// — reaching for mu here would be the two lock orders that deadlock, since
	// negotiate takes mu and then reaches for that same emulator lock. Nothing
	// is woken from them either: what they report only ever changes as part of
	// output, and sink wakes everybody the moment that write returns.
	name := target.Name()
	em.SetCallbacks(vt.Callbacks{
		IconName: func(title string) {
			log.Debug("terminal tab title", "container", name, "title", title)
			s.setTitle(title)
		},
		Title: func(title string) {
			log.Debug("terminal window title", "container", name, "title", title)
		},
		WorkingDirectory: func(dir string) {
			log.Debug("terminal working directory", "container", name, "dir", dir)
		},
		Bell:      func() { log.Debug("terminal bell", "container", name) },
		AltScreen: func(on bool) { log.Debug("terminal alternate screen", "container", name, "on", on) },
		CursorVisibility: func(visible bool) {
			log.Debug("terminal cursor visibility", "container", name, "visible", visible)
		},
		CursorStyle: func(style vt.CursorStyle, blink bool) {
			log.Debug("terminal cursor style", "container", name, "style", style, "blink", blink)
		},
		CursorColor: func(c color.Color) {
			log.Debug("terminal cursor colour", "container", name, "colour", c)
		},
		ForegroundColor: func(c color.Color) {
			log.Debug("terminal foreground colour", "container", name, "colour", c)
		},
		BackgroundColor: func(c color.Color) {
			log.Debug("terminal background colour", "container", name, "colour", c)
		},
		EnableMode:  func(m ansi.Mode) { log.Debug("terminal mode enabled", "container", name, "mode", m) },
		DisableMode: func(m ansi.Mode) { log.Debug("terminal mode disabled", "container", name, "mode", m) },
	})

	// The emulator answers what a real terminal answers — a device-attributes
	// query, a cursor-position report — and those replies have to reach the
	// app or they pile up until the emulator stops accepting output at all.
	// That is a deadlock, not a slow path: the write that fills the buffer
	// never returns, so nothing is ever drawn again. Copying them into the
	// same stdin the viewers type on is what a terminal does with them.
	//
	// A viewer's keystrokes arrive here too, by the same route: the frame
	// hands the emulator the key and the emulator encodes it, so what the
	// container reads is encoded by something that knows the modes the app has
	// set rather than by a browser that does not.
	go func() {
		if _, err := io.Copy(pw, em); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			log.Debug("terminal replies stopped", "container", target.Name(), "error", err)
		}
	}()

	go func() {
		defer close(s.done)
		defer s.wakeAll()
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

// eof asks the target to end, which is what a shell does with an end of file.
// It is the deliberate half of the guard the frame puts on Ctrl-D: the key no
// longer ends a shared session by accident, and this is how somebody ends one
// on purpose.
func (s *session) eof() {
	if _, err := s.stdin.Write([]byte{0x04}); err != nil {
		s.log.Debug("end of file not delivered", "container", s.Name(), "error", err)
	}
}

// sink is where the target's output lands: into the emulator, which is the
// screen, and a nudge to everyone drawing it.
type sink struct{ s *session }

func (w *sink) Close() error { return nil }

func (w *sink) Write(p []byte) (int, error) {
	s := w.s

	// The screen first, then the people drawing it. A frame woken before the
	// emulator had the bytes would render the screen as it was and not be
	// asked again.
	_, _ = s.em.Write(p)
	s.wakeAll()
	return len(p), nil
}

// wakeAll asks every viewer's frame to render again. The sends are off the
// lock, and each is a drop rather than a wait, so neither a slow frame nor a
// slow socket can hold up the emulator.
func (s *session) wakeAll() {
	s.mu.Lock()
	wake := make([]chan struct{}, 0, len(s.viewers))
	for v := range s.viewers {
		wake = append(wake, v.wake)
	}
	s.mu.Unlock()

	for _, w := range wake {
		select {
		case w <- struct{}{}:
		default:
		}
	}
}

// AttachContainer joins a viewer to the shared session, which is what
// ServeAttach calls once per websocket.
//
// It does not reach the target at all. The viewer gets a frame of its own,
// rendering the shared screen, and its window size is applied whenever it
// arrives rather than waited for: a client that never sends one still gets a
// working terminal at whatever size the session already settled on, and the
// page's own size lands a moment later and redraws. Blocking on it here would
// hang any client that does not send one, which the streaming protocol does
// not require.
//
// The resize channel closing is how the socket says it is gone. ServeAttach
// opens that stream unconditionally and closes it when the connection ends,
// which makes it the one signal that works whether the page closed cleanly or
// the laptop lid did.
func (s *session) AttachContainer(ctx context.Context, _, _, _ string, in io.Reader, out, _ io.WriteCloser, _ bool, resize <-chan remotecommand.TerminalSize) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	width, height := s.window()

	v := &viewer{wake: make(chan struct{}, 1)}
	v.prog = tea.NewProgram(
		frame{sess: s, v: v, width: width, height: height},
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(out),
		// Stated outright, because there is nothing here to detect it from.
		// The output is a websocket rather than a terminal, so detection
		// answers NoTTY, and NoTTY strips every escape on the way out — the
		// container's colours, its bold, and the frame's own dim status line
		// with them, leaving the whole screen monochrome. The environment
		// cannot rescue it either: COLORTERM only upgrades a profile that is
		// not already NoTTY.
		//
		// TrueColor because the terminal at the other end is xterm.js, and
		// because it is the profile that passes what the app emitted through
		// unchanged instead of quantising it on the way past.
		tea.WithColorProfile(colorprofile.TrueColor),
		// TERM still describes that terminal, for everything about it that is
		// not colour.
		tea.WithEnvironment([]string{"TERM=xterm-256color", "COLORTERM=truecolor"}),
	)

	s.join(v)
	defer s.part(v)

	go s.follow(ctx, cancel, v, resize)
	go s.redraw(ctx, v)

	if _, err := v.prog.Run(); err != nil && ctx.Err() == nil {
		s.log.Debug("frame ended", "container", s.Name(), "error", err)
	}
	return nil
}

// redraw turns the wake channel into the message the frame updates on. It is a
// goroutine rather than a direct send from sink so that a frame busy rendering
// never blocks the emulator behind it.
func (s *session) redraw(ctx context.Context, v *viewer) {
	for {
		select {
		case <-v.wake:
			select {
			case <-s.done:
				v.prog.Send(goneMsg{})
				return
			default:
			}
			v.prog.Send(paneMsg{})
		case <-ctx.Done():
			return
		}
	}
}

// join registers a viewer. Nothing is replayed to it: its frame is new, so its
// first render is a whole one, drawn from the emulator as it stands.
func (s *session) join(v *viewer) {
	s.mu.Lock()
	s.viewers[v] = struct{}{}
	s.mu.Unlock()

	// Everyone else's status line just became wrong: it names how many are
	// watching, and that is now one more. Nothing else would tell them until
	// the container next said something, which on a quiet shell is never.
	s.wakeAll()
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
		s.wakeAll() // one fewer viewer, and every status line says so
	}
}

// resizeViewer records what one viewer's window is now and settles the session
// on it. The frame calls this, because the frame is what learns the size.
func (s *session) resizeViewer(v *viewer, width, height int) {
	s.mu.Lock()
	v.size = remotecommand.TerminalSize{Width: uint16(width), Height: uint16(height)}
	negotiated := s.negotiate()
	s.mu.Unlock()
	s.apply(context.Background(), negotiated)
}

// negotiate settles the pty on the smallest window watching it, and resizes
// the emulator to match. Smallest rather than newest or first, because the pty
// has one size and every viewer renders all of it: anything larger than the
// smallest window is drawn wrapped or clipped there, which looks exactly like
// the corruption this whole session exists to prevent. A larger window gets
// unused margin instead, which is merely wasteful.
//
// What the target is told is the pane, not the window: the frame keeps rows of
// its own, and a container sized to the whole window would draw its last rows
// underneath the status line.
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

	// A window with no room for the pane still has to leave the emulator a
	// screen: one resized to nothing has nowhere to put what the container
	// says next.
	pane := remotecommand.TerminalSize{Width: w - chromeWidth, Height: h - chromeHeight}
	if h <= chromeHeight {
		pane.Height = 1
	}
	if w <= chromeWidth {
		pane.Width = 1
	}
	s.em.Resize(int(pane.Width), int(pane.Height))
	return pane
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

// follow tracks one viewer's window size, and treats the channel closing as
// the socket having gone.
//
// The size goes to the frame rather than straight to negotiate, because the
// frame needs it too — it is drawing into that window — and a size that
// reached the session by one route and the frame by another is a size the two
// could disagree about. The frame hands it back through resizeViewer.
func (s *session) follow(ctx context.Context, cancel context.CancelFunc, v *viewer, resize <-chan remotecommand.TerminalSize) {
	defer cancel()
	for {
		select {
		case size, ok := <-resize:
			if !ok {
				return
			}
			v.prog.Send(tea.WindowSizeMsg{Width: int(size.Width), Height: int(size.Height)})
		case <-ctx.Done():
			return
		case <-s.done:
			return
		}
	}
}

// window is the window the session has settled on. A frame starts at it, so a
// viewer joining one that two other people are already watching draws at the
// size they are watching rather than repainting a moment later.
func (s *session) window() (int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return int(s.size.Width), int(s.size.Height)
}

// setTitle records what the shell last called this terminal.
func (s *session) setTitle(title string) {
	s.titleMu.Lock()
	s.title = title
	s.titleMu.Unlock()
}

// titled is what the shell last called this terminal, or "" if it has never
// said. It is the command's name while one is running, on a shell that reports
// one at all.
func (s *session) titled() string {
	s.titleMu.Lock()
	defer s.titleMu.Unlock()
	return s.title
}

// announce records the public address this origin answers on, and has every
// frame say so.
//
// It arrives after the servers do, and cannot not: a container is bound before
// the tunnel is minted, because the binding is what the tunnel is given to
// proxy to. So a frame drawn in between has nothing to put in its corner, and
// this is what fills it in when there is finally something to say.
func (s *session) announce(public string) {
	s.mu.Lock()
	s.public = public
	s.mu.Unlock()
	s.wakeAll()
}

// announced is the public address, or "" before the tunnel has said.
func (s *session) announced() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.public
}

// count is how many viewers are watching, for the frame to say so.
func (s *session) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.viewers)
}

// sendKey gives a keystroke to the container, encoded by the emulator. See
// asKeyEvent for why it goes this way round rather than as bytes.
//
// A key that produced text is sent as that text. The alternative re-derives it
// from the code and the modifiers, and a shifted letter is exactly where that
// goes wrong: a capital arrives as the unshifted rune plus Shift, so what comes
// back out is a modified key rather than a letter, and every capital the person
// typed disappears on the way to the shell.
func (s *session) sendKey(k tea.Key) {
	if k.Text != "" {
		s.em.SendText(k.Text)
		return
	}
	s.em.SendKey(asKeyEvent(k))
}

// drawPane has the emulator draw its screen into area of scr. The frame calls
// it rather than reading lines back, so the cells arrive as cells.
func (s *session) drawPane(scr uv.Screen, area uv.Rectangle) { s.em.Draw(scr, area) }

// paste hands pasted text to the container, bracketed if the app inside asked
// for that.
//
// Through the emulator rather than written straight to stdin, for the same
// reason a keystroke goes that way: the emulator is what read the app's
// \x1b[?2004h and so is the only thing here that knows whether this app wants
// its pastes bracketed. Text written past it arrives as though it had been
// typed, which is exactly what bracketed paste exists to stop — a shell cannot
// tell a pasted newline from a pressed one, and runs the half-finished command.
func (s *session) paste(text string) { s.em.Paste(text) }

// paneSize is the screen the container is drawing on.
func (s *session) paneSize() (int, int) { return s.em.Width(), s.em.Height() }

// paneCursor is where the app inside believes the cursor is.
func (s *session) paneCursor() uv.Position { return s.em.CursorPosition() }

// scrollUp moves the pane one line further back, stopping where the emulator's
// own scrollback does. Reported rather than held here: how far back a viewer
// is looking is that viewer's, and two of them scroll independently over the
// one screen.
func (s *session) scrollUp(from int) int {
	if from >= s.em.ScrollbackLen() {
		return from
	}
	return from + 1
}

// paneLines is the screen as rows of text, scrolled back by scroll lines and
// clipped to rows.
//
// Rendered rather than replayed. The emulator holds what the screen is, so a
// frame asks it every time it draws instead of keeping a copy that a write it
// missed would make wrong.
func (s *session) paneLines(scroll, rows int) []string {
	if rows <= 0 {
		return nil
	}
	lines := strings.Split(s.em.Render(), "\n")

	if scroll > 0 {
		back := s.em.Scrollback().Lines()
		if scroll > len(back) {
			scroll = len(back)
		}
		scrolled := make([]string, 0, scroll+len(lines))
		for _, line := range back[len(back)-scroll:] {
			scrolled = append(scrolled, line.Render())
		}
		lines = append(scrolled, lines...)
	}

	if len(lines) > rows {
		lines = lines[:rows]
	}
	return lines
}
