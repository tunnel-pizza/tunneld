package attach

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"strings"
	"sync"

	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/colorprofile"
	uv "github.com/charmbracelet/ultraviolet"
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

	// quit says a viewer asked the whole run to end. What it reaches is the
	// Server, which says so to the command; nothing here acts on it, because
	// what a frame can end is its own viewer and what this ends is a process.
	quit func()

	// ctx is the origin's own lifetime, from Serve. Held because a run can
	// outlive the viewer who started it and a later run needs a context that
	// is not some departed socket's — see revive.
	ctx context.Context

	log *slog.Logger

	// banner is the build line the frame shows along the bottom. Fixed for the
	// life of the process, so it is read without the lock.
	banner string

	// logs is tunneld's own recent lines, for the frame to show on request.
	// Nil when nothing was configured, which a frame says rather than hides.
	logs Logs

	// sinks are told what the terminal says about itself, after the built-in
	// routing. Empty unless a caller installed some with WithSinks.
	sinks []Sink

	// stdin is the write end of the pipe feeding the target, and done closes
	// when the run reading it is over. Both belong to one run and are replaced
	// by the next, so both are guarded by mu — read stdin and done through the
	// lock, never off the field.
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

	// scan pulls the sequences a program uses to talk about itself out of its
	// output before the emulator sees them. Built by watch, reset by revive.
	scan *scanner

	// titleMu guards the title and subtitle and nothing else. said writes them
	// from the stream goroutine, a frame's titles() reads them from its own,
	// and titleMu is deliberately not mu so neither waits behind viewer or
	// size bookkeeping it has nothing to do with.
	titleMu  sync.Mutex
	title    string
	subtitle string

	// cursorMu guards hidden, and is separate from titleMu for the same reason
	// titleMu is separate from mu: said writes it from the stream goroutine
	// and a frame's cursorHidden() reads it from its own, and the two fields
	// have nothing to do with each other.
	//
	// hidden is what the program asked for with DECTCEM. A full-screen program
	// hides the cursor once, at startup, and then leaves it wherever its last
	// write ended — so a frame that draws one anyway shows a cursor skating
	// around the screen on every redraw.
	cursorMu sync.Mutex
	hidden   bool

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
	// said carries sequences the emulator has no use for to this viewer's
	// terminal. Buffered, and a send that finds it full drops: a viewer that
	// has stopped reading holds nobody else up, the rule wake already follows.
	said chan []byte
}

// watch builds the scanner that reads what a program says about itself, ahead
// of the emulator. Split out of newSession so a frame test can drive a session
// the way a real stream does.
//
// Nothing about the terminal reaches the frame through vt's callbacks any
// more: they fire on data the emulator's own parser already cut, they cannot
// tell one OSC 0 from an OSC 1 and an OSC 2, and they run under the emulator's
// lock. The scanner owns every OSC and reports every private mode, and said
// routes them.
func (s *session) watch() {
	s.scan = newScanner(s.em, s.said)
}

// newSession opens the one attach and starts feeding the emulator from it. It
// returns as soon as the stream is running; a target that fails is reported
// through the log, because by this point the tunnel is already up and a dead
// terminal origin is not worth taking it down.
func newSession(ctx context.Context, target Target, banner string, logs Logs, sinks []Sink, quit func(), log *slog.Logger) *session {
	em := vt.NewSafeEmulator(defaultCols, defaultRows)

	s := &session{
		Target:  target,
		ctx:     ctx,
		quit:    quit,
		log:     log,
		banner:  banner,
		logs:    logs,
		sinks:   sinks,
		resize:  make(chan remotecommand.TerminalSize),
		em:      em,
		viewers: map[*viewer]struct{}{},
		size:    remotecommand.TerminalSize{Width: defaultCols, Height: defaultRows},
	}

	s.watch()

	s.mu.Lock()
	s.stream()
	s.mu.Unlock()
	return s
}

// stream opens one attach and starts feeding the emulator from it. The caller
// holds mu.
//
// Everything it builds belongs to this run and not to the session: the pipe
// the target reads, the goroutine draining the emulator's replies into it, and
// the channel that says the run is over. A second run is a second set, which
// is what makes revive possible at all — the first run's pipe is closed and
// its done is closed, and neither can be reused.
func (s *session) stream() {
	pr, pw := io.Pipe()
	done := make(chan struct{})
	s.stdin, s.done = pw, done

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
		if _, err := io.Copy(pw, s.em); err != nil && !errors.Is(err, io.ErrClosedPipe) {
			s.log.Debug("terminal replies stopped", "container", s.Name(), "error", err)
		}
	}()

	go func() {
		defer close(done)
		defer s.wakeAll()
		defer func() { _ = pr.Close() }()

		name := s.Name()
		out := &sink{s: s}
		// s.Target, not s, and the spelling is load-bearing: a session has an
		// AttachContainer of its own — the per-viewer one — and calling that
		// here would have the session attach to itself.
		if err := s.Target.AttachContainer(s.ctx, name, "", name, pr, out, out, s.TTY(), s.resize); err != nil &&
			s.ctx.Err() == nil {
			s.log.Debug("attach session ended", "container", name, "error", err)
		}
	}()

	// Tell the run how big its terminal is, whether or not anything changed.
	//
	// A run starts at whatever size its target made — for a pty, the system's
	// default — and negotiate only speaks when the window moves. The first run
	// is told because a viewer arriving is a window where there was none; a
	// second run is told by nobody, since the viewer who asked for it is the
	// same size the session already settled on. Without this it draws into a
	// size no one is looking at, which for a full-screen program means drawing
	// nothing at all.
	go s.apply(s.ctx, paneOf(s.size))
}

// logLines are tunneld's own recent lines, oldest first, or nil when nothing
// is keeping them.
func (s *session) logLines() []string {
	if s.logs == nil {
		return nil
	}
	return s.logs.Lines()
}

// recoverable reports whether this target can be started again, which is what
// decides whether a keystroke that ends it is worth standing in front of.
//
// A program can: its origin is a path, so the frame lets Ctrl-C and Ctrl-D
// through untouched and the worst anybody does is have to open the page again.
// A container cannot: once its PID 1 has exited there is nothing to attach to,
// and the same keystroke is the end of the terminal for everybody watching.
func (s *session) recoverable() bool {
	again, ok := s.Target.(Repeatable)
	return ok && again.Repeatable()
}

// What a viewer arriving now would get, and the words the page puts on the
// button that gets it.
const (
	// offerReconnect is a run still going: the same terminal, still there,
	// with whatever was on it.
	offerReconnect = "reconnect"
	// offerRestart is a run that has ended and a target that can be started
	// again: a new program on a clean screen, which is a different thing and
	// says so.
	offerRestart = "restart"
)

// offer is what coming back would do, or "" when nothing would.
//
// Asked at the moment somebody's socket has gone rather than when the page was
// built, because the answer changes and the page is long-lived: a viewer who
// detaches from a running shell is reconnecting to it, and the same viewer an
// hour later, after the shell has exited, is starting a new one. A page that
// decided this at load time would tell one of them the wrong thing.
func (s *session) offer() string {
	select {
	case <-s.ended():
		if s.recoverable() {
			return offerRestart
		}
		return ""
	default:
		return offerReconnect
	}
}

// ended is the channel that closes when the current run is over. Read through
// a lock rather than off the field, because revive replaces it: a run that has
// finished and a run that is about to start are two different channels, and a
// goroutine left over from the first must not be reading the second's.
func (s *session) ended() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done
}

// revive starts the target again for a viewer arriving after the last run
// ended, when the target is one that can be.
//
// A program origin is the case this exists for: the origin is a path, so
// running it a second time is exactly as well defined as running it the first,
// and without this the origin serves a screen that can never produce another
// byte the moment its program exits — which for `tunneld ls` is before anybody
// opens the page. A container is not repeatable and says so by not
// implementing Repeatable: once its PID 1 has exited there is nothing left to
// attach to.
//
// The screen goes with the run that drew it. RIS rather than a fresh emulator,
// so every viewer keeps drawing the same one, and the scrollback with it: this
// is a new program, and a screen carrying the last one's output would be
// claiming a state the target was never in.
func (s *session) revive() {
	s.mu.Lock()
	defer s.mu.Unlock()

	select {
	case <-s.done:
	default:
		return // still running; a viewer is joining, not restarting
	}
	if s.ctx.Err() != nil {
		return // the origin itself is going away
	}
	again, ok := s.Target.(Repeatable)
	if !ok || !again.Repeatable() {
		return
	}

	_, _ = s.em.Write([]byte("\x1bc"))
	s.em.ClearScrollback()
	s.scan.reset()
	s.log.Info("running it again", "target", s.Name())
	s.stream()
}

// close ends the shared attach. The pipe is what the target is reading, so
// closing it is what unblocks it.
func (s *session) close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stdin.Close()
}

// endRun says a viewer has asked the whole run to end. What it reaches is the
// Server, which tells the command; the command is what actually ends, taking
// its context and everything started under it.
//
// Not the target and not this origin. A frame can end its own viewer and it
// can ask for this, and the difference is worth keeping: one of them is a tab
// closing and the other is a process exiting.
func (s *session) endRun() {
	if s.quit != nil {
		s.quit()
	}
}

// sink is where the target's output lands: into the emulator, which is the
// screen, and a nudge to everyone drawing it.
type sink struct{ s *session }

func (w *sink) Close() error { return nil }

func (w *sink) Write(p []byte) (int, error) {
	s := w.s
	// Through the scanner, which writes the screen bytes to the emulator and
	// hands every OSC and private mode to said. A frame woken before the
	// emulator had the bytes would render the screen as it was, so the wake is
	// after.
	_, _ = s.scan.Write(p)
	s.wakeAll()
	return len(p), nil
}

// names is the OSC codes that name the terminal: 0 sets both, 1 the subtitle,
// 2 the title. tunneld owns these — the emulator never sees them.
func names(cmd int) bool { return cmd == 0 || cmd == 1 || cmd == 2 }

// paints is the OSC codes the emulator acts on: hyperlinks and the colour
// set/query pairs. It is vt's registerDefaultOscHandlers minus what we own
// (names, and 7 the working directory). A vt bump that adds a handler is a code
// the emulator has started painting and this list is silently withholding — so
// re-check it against registerDefaultOscHandlers when bumping x/vt.
func paints(cmd int) bool {
	switch cmd {
	case 8, 10, 11, 12, 110, 111, 112:
		return true
	}
	return false
}

// said routes one thing the terminal said about itself. It runs on the stream
// goroutine and takes no emulator lock: an OSC is delivered here instead of
// being written to the screen, and a Mode after its write has already
// returned.
func (s *session) said(seq Sequence) {
	switch seq.Kind {
	case Mode:
		// The CSI is already on the screen; only the cursor is the frame's
		// business, and only DECTCEM among the modes.
		if seq.Cmd == 25 { // DECTCEM
			s.setCursorHidden(!seq.Set)
		}
	case OSC:
		switch {
		case names(seq.Cmd):
			s.setName(seq.Cmd, string(seq.Data))
		case paints(seq.Cmd) || seq.Cmd < 0:
			// The emulator's, verbatim: a hyperlink becomes cell links it
			// draws, a colour query one it answers. An OSC with no numeric
			// command is not ours to judge, so it goes there too.
			_, _ = s.em.Write(seq.Raw)
		default:
			// Nothing here acts on it and the emulator has no use for it, but
			// a real terminal on the far end of the tunnel might: the
			// clipboard, a notification, shell integration. Send it there.
			s.forward(seq.Raw)
		}
	}

	s.log.Debug("terminal said", "container", s.Name(),
		"kind", seq.Kind, "cmd", seq.Cmd, "data", string(seq.Data), "set", seq.Set)

	for _, sk := range s.sinks {
		sk.Said(seq)
	}
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

// forward hands a sequence to every viewer's terminal. The copy is because raw
// is the scanner's buffer, valid only for the said call; the channel outlives
// it. A full channel is a drop, like wake: the container's output does not wait
// on a viewer that has stopped reading.
func (s *session) forward(raw []byte) {
	cp := append([]byte(nil), raw...)
	s.mu.Lock()
	chans := make([]chan []byte, 0, len(s.viewers))
	for v := range s.viewers {
		chans = append(chans, v.said)
	}
	s.mu.Unlock()

	for _, ch := range chans {
		select {
		case ch <- cp:
		default:
			s.log.Debug("dropped a forwarded sequence for a slow viewer", "container", s.Name())
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

	// A viewer arriving after the last run ended starts the next one, when the
	// target is something that can be started again. Nothing else can do it:
	// the run ending drops every viewer there was, so the person who opens the
	// page afterwards is the only one left to ask.
	s.revive()

	width, height := s.window()

	v := &viewer{wake: make(chan struct{}, 1), said: make(chan []byte, 64)}
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

// viewLocally puts a viewer on the console tunneld was started from, which is
// the same viewer a browser gets and joins the same session: one emulator, one
// viewer count, keystrokes interleaved.
//
// Two things a page needs are left out. The colour profile and TERM are stated
// outright for a websocket because detection through a socket answers NoTTY; a
// real terminal describes itself and is allowed to. And sizes arrive from
// SIGWINCH rather than from a resize channel, which is Bubble Tea's own job on
// a real terminal — so follow is given none, and stays only for what it does
// besides: ending this viewer when the run does.
//
// Returns when the viewer leaves or the run ends. The console is restored
// either way, which is Bubble Tea's doing and the reason detaching has to go
// through it rather than around it.
func (s *session) viewLocally(ctx context.Context, in io.Reader, out io.Writer) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	s.revive()
	width, height := s.window()

	v := &viewer{wake: make(chan struct{}, 1), said: make(chan []byte, 64)}
	v.prog = tea.NewProgram(
		frame{sess: s, v: v, width: width, height: height},
		tea.WithContext(ctx),
		tea.WithInput(in),
		tea.WithOutput(out),
	)

	s.join(v)
	defer s.part(v)

	go s.follow(ctx, cancel, v, nil)
	go s.redraw(ctx, v)

	_, err := v.prog.Run()
	return err
}

// redraw turns the wake channel into the message the frame updates on. It is a
// goroutine rather than a direct send from sink so that a frame busy rendering
// never blocks the emulator behind it.
func (s *session) redraw(ctx context.Context, v *viewer) {
	for {
		select {
		case <-v.wake:
			// Draw first, end second. A wake carries output, and the wake that
			// arrives with the run's last output is also the one that finds
			// the run over — so a frame told to quit on it would drop exactly
			// the bytes a reader most wants: what the program said on its way
			// out. A program that exits quickly is all last words.
			v.prog.Send(paneMsg{})
			select {
			case <-s.ended():
				v.prog.Send(goneMsg{})
				return
			default:
			}
		case raw := <-v.said:
			// Straight to the viewer's terminal, unmanaged by the renderer.
			// An OSC carries no cells, so where it lands between two frames
			// does not matter.
			v.prog.Send(tea.RawMsg{Msg: string(raw)})
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

	pane := paneOf(s.size)
	s.em.Resize(int(pane.Width), int(pane.Height))
	return pane
}

// paneOf is the screen inside a window: the window less the frame's own
// border.
//
// A window with no room for the pane still leaves a screen, because one
// resized to nothing has nowhere to put what the target says next.
func paneOf(window remotecommand.TerminalSize) remotecommand.TerminalSize {
	pane := remotecommand.TerminalSize{
		Width:  window.Width - chromeWidth,
		Height: window.Height - chromeHeight,
	}
	if window.Height <= chromeHeight {
		pane.Height = 1
	}
	if window.Width <= chromeWidth {
		pane.Width = 1
	}
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
	case <-s.ended():
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
		case <-s.ended():
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

// setTitle records the window title, which a prompt framework sets to the whole
// command line. What arrives is whole — the scanner never cuts it — so there is
// nothing to guard against but an app that genuinely sends garbage, which
// frame.showable handles when it draws.
func (s *session) setTitle(title string) {
	s.titleMu.Lock()
	s.title = title
	s.titleMu.Unlock()
}

// setSubtitle records the tab title, the shorter name beside it.
func (s *session) setSubtitle(subtitle string) {
	s.titleMu.Lock()
	s.subtitle = subtitle
	s.titleMu.Unlock()
}

// setName applies an OSC that names the terminal: 0 sets both, 2 the title, 1
// the subtitle.
func (s *session) setName(cmd int, name string) {
	if cmd == 0 || cmd == 2 {
		s.setTitle(name)
	}
	if cmd == 0 || cmd == 1 {
		s.setSubtitle(name)
	}
}

// setCursorHidden records what the program asked for with DECTCEM. Called
// from said on the stream goroutine, which is why it takes a lock of its own:
// a frame reads cursorHidden from its own goroutine, concurrently.
func (s *session) setCursorHidden(hidden bool) {
	s.cursorMu.Lock()
	s.hidden = hidden
	s.cursorMu.Unlock()
}

// cursorHidden reports whether the program has asked for no cursor. A frame
// asks before drawing one: the emulator keeps a position whether or not
// anything should be shown there, and a full-screen program leaves that
// position wherever its last write ended.
func (s *session) cursorHidden() bool {
	s.cursorMu.Lock()
	defer s.cursorMu.Unlock()
	return s.hidden
}

// titles are the two names the terminal goes by, either "" if it has never
// said. A prompt framework sets the title to the running command's whole line
// and the subtitle to its name; an app that sets them with one OSC 0 sets both
// to the same thing.
func (s *session) titles() (title, subtitle string) {
	s.titleMu.Lock()
	defer s.titleMu.Unlock()
	return s.title, s.subtitle
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

// paneLines is the screen as rows of text, clipped to rows.
//
// Rendered rather than replayed. The emulator holds what the screen is, so a
// caller asks it every time instead of keeping a copy that a write it missed
// would make wrong.
func (s *session) paneLines(rows int) []string {
	if rows <= 0 {
		return nil
	}
	lines := strings.Split(s.em.Render(), "\n")
	if len(lines) > rows {
		lines = lines[:rows]
	}
	return lines
}
