package attach

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// host is the machine tunneld is running on, asked for once. It cannot change
// while the process does, and a frame draws several times a second.
//
// A host that will not say its own name is not worth reporting an error over:
// the frame simply has nothing to put there.
var host = sync.OnceValue(func() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}
	return name
})

// What the frame keeps for itself: a border, and the two rows and two columns
// it occupies.
//
// The terminal page's own rule is that the terminal is the page, because
// anything framing it steals rows from a viewport that is often a phone. This
// is the whole of what is spent — the title rides in the top border and the
// keys in the bottom one, rather than either taking a row of its own.
const (
	chromeHeight = 2
	chromeWidth  = 2
)

// The frame's palette, indexed rather than true colour so it lands the same
// way on a terminal that has only 256 of them. Light blue border, a cyan name
// with its qualifier in magenta and its measurements in white, and a chip in
// black on orange for the key that opens the commands.
var (
	borderStyle = uv.Style{Fg: ansi.IndexedColor(111)}
	hostStyle   = uv.Style{Fg: ansi.IndexedColor(245)}
	bannerStyle = uv.Style{Fg: ansi.IndexedColor(240)}
	addrStyle   = uv.Style{Fg: ansi.IndexedColor(114)}
	nameStyle   = uv.Style{Fg: ansi.IndexedColor(45), Attrs: uv.AttrBold}
	titleStyle  = uv.Style{Fg: ansi.IndexedColor(179)}
	qualStyle   = uv.Style{Fg: ansi.IndexedColor(207)}
	countStyle  = uv.Style{Fg: ansi.IndexedColor(255)}
	chipStyle   = uv.Style{Fg: ansi.IndexedColor(232), Bg: ansi.IndexedColor(214), Attrs: uv.AttrBold}
	backStyle   = uv.Style{Fg: ansi.IndexedColor(232), Bg: ansi.IndexedColor(75), Attrs: uv.AttrBold}
	copyStyle   = uv.Style{Fg: ansi.IndexedColor(232), Bg: ansi.IndexedColor(114), Attrs: uv.AttrBold}
	hintStyle   = uv.Style{Fg: ansi.IndexedColor(245)}
	// logStyle is dimmer than the terminal it covers, because what it shows is
	// tunneld talking about itself rather than the thing anybody came to see.
	logStyle = uv.Style{Fg: ansi.IndexedColor(245)}
	qrStyle  = uv.Style{Fg: ansi.IndexedColor(255)}
)

// How long a frame waits to be told how big its window is before drawing at
// the size the session settled on instead.
//
// The page sends its size the moment its socket opens, so this is only ever
// spent on something that is not the page. Half a second is longer than the
// round trip through the edge and short enough that a client which never sends
// one is not left staring at nothing.
const sizeGrace = 500 * time.Millisecond

// armGrace is how long a session-ending key stays armed. Long enough to read
// the border and answer it, short enough that walking away disarms it: a
// second press minutes later is a new intention, not the other half of a
// double tap.
const armGrace = 2 * time.Second

// disarmMsg is an arming expiring. It carries the arming it belongs to, so a
// tick from one that was already spent cannot clear the next one — press,
// type on, press again inside two seconds, and the stale tick would otherwise
// disarm a key the viewer had just armed.
type disarmMsg struct{ arming int }

// settleMsg is the grace period expiring: draw at the session's size, since
// whoever is watching has not said what theirs is.
type settleMsg struct{}

// paneMsg says the container wrote something. The frame holds no copy of the
// screen — the emulator is the screen — so the message carries nothing: it
// only says that rendering again is worth doing.
type paneMsg struct{}

// goneMsg says the shared stream has ended. Every viewer's frame gets one,
// because the session is over for all of them at once.
type goneMsg struct{}

// frame is one viewer's view of the shared pane: the container's screen, a
// status line under it, and the command mode Ctrl-D opens.
//
// One frame per viewer rather than one shared between them, which is what
// makes a per-viewer command mode possible at all — a shared model would put
// every other viewer into command mode when one of them pressed Ctrl-D. It is
// also what removes the need to replay a composed screen to a viewer arriving
// late: its frame is new, so its first render is a whole one, and bubbletea's
// differential renderer is measuring against a screen it drew itself.
//
// The pane is shared, and stays the single screen of record. Every frame
// renders the same emulator, so what two viewers see differs only in the
// chrome each of them is driving.
type frame struct {
	sess *session
	v    *viewer

	// The viewer's window, as it last reported it. The box the frame draws
	// is this or smaller: the session settles the emulator on the smallest
	// viewer's window, and a larger window draws the box at that size with
	// nothing around it. See box.
	width, height int

	// command is the frame's key having been pressed: the next keystroke
	// belongs to the frame and the container will not see it.
	command bool

	// armed is a session-ending control key waiting to be asked for a second
	// time — 'c' or 'd', or zero when none is. Only ever set for a target
	// that cannot be started again; see the guard in Update.
	//
	// arming counts them, so the tick that expires one can tell whether it is
	// still the one that is armed.
	armed  rune
	arming int

	// logs is the frame showing tunneld's own lines over the pane instead of
	// the terminal. A view rather than a keystroke: it stays until esc, since
	// reading is not something anybody finishes in one key.
	logs bool

	// qr is the frame showing the public address as a code a phone can read,
	// over the pane. A view for the same reason logs is: pointing a camera at
	// a screen takes longer than a keystroke.
	qr bool

	// sized is the window having been reported by the page rather than assumed.
	// Until it is, the frame draws nothing rather than drawing at a size that
	// is somebody else's.
	sized bool

	// scrolled is this viewer reading history rather than the live screen,
	// and top is which line of the emulator's history is the pane's first
	// row while they are. Per viewer, because the screen is shared and two
	// people can be looking back different distances at it.
	//
	// An index into the history rather than a distance from the bottom, so
	// that output arriving while somebody reads does not slide what they are
	// reading out from under them: the pager's rule, not the terminal's. The
	// distance grows instead, and the border shows it.
	scrolled bool
	top      int

	// sel is what this viewer has dragged across, in pane coordinates, and
	// selecting is the button still being down. selected is a finished drag
	// that is still shown: it stays highlighted, so what was copied can be
	// seen, until a key, a wheel or another click.
	//
	// The frame's own, because the terminal has handed the mouse over — see
	// View — and a terminal that is not doing selection leaves nobody but
	// the frame to do it. The same on a console and in a tab: xterm stops
	// selecting once an application asks for the mouse, exactly as a
	// terminal does.
	sel       selection
	selecting bool
	selected  bool

	// linger is this frame staying on the screen after the run ends, until a
	// key, rather than quitting with it. Set for the console: a tab has the
	// page to say "ended" and offer a way back, and a console has nothing
	// under the frame but a prompt. Quitting at once would also lose the
	// last screen — the line that says why a program exited — and hand the
	// terminal back before it has answered the queries Bubble Tea sent at
	// startup, which then land on the prompt as text. ended is the run
	// having ended while lingering.
	linger bool
	ended  bool

	// embedded is this viewer being a tile in the multiview panel, which
	// already shows what the provider said, once, in its own bar above every
	// tile. A frame that drew the bar as well would put the same warning on the
	// page once per terminal plus the panel's own, so an embedded frame draws
	// none. The page says which it is on the socket's path; see index.html.
	embedded bool
}

// Init asks for nothing. The first render happens as soon as the program
// starts, and everything after it is driven by the pane and the keyboard.
func (f frame) Init() tea.Cmd { return nil }

// Update routes a keystroke to the frame or to the container, and nowhere
// else. Ctrl-D is the one key the container never sees: it is what a viewer
// presses to reach the frame, and it is also what a shell reads as end of
// file, so leaving it through means any viewer can close the shared shell for
// everybody with a keystroke they meant for their own session.
func (f frame) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		// The renderer measures its terminal at startup and reports what it
		// found. There is no terminal here to measure — the output is a
		// websocket — so what it finds is nothing, and a renderer that
		// believes it has no rows draws none of them.
		//
		// Waited out rather than answered at once. The session's settled size
		// is a guess about somebody else's window, and drawing at it means the
		// viewer's first frame is a box of the wrong size with the cursor
		// somewhere inside it, replaced a moment later when the page says how
		// big it actually is. Better to draw nothing for that moment: the
		// renderer paints nothing at zero, which is exactly the right amount.
		//
		// The guess is still made, just late, and it is still made as a
		// message rather than pushed in from outside — whichever of the two
		// landed second would win, and when that is the zero the frame never
		// draws again.
		if msg.Width == 0 || msg.Height == 0 {
			return f, tea.Tick(sizeGrace, func(time.Time) tea.Msg { return settleMsg{} })
		}
		f.width, f.height = msg.Width, msg.Height
		f.sized = true
		f.sess.resizeViewer(f.v, msg.Width, msg.Height)
		return f, nil

	case disarmMsg:
		// Only if this is still the arming that tick belongs to.
		if msg.arming == f.arming {
			f.armed = 0
		}
		return f, nil

	case settleMsg:
		// Whatever size is known by now, said again so the renderer has it.
		// A page that answered in time has already set one and this restates
		// it; one that never will gets the session's, which is what keeps a
		// client that does not speak the resize protocol from staring at an
		// empty terminal.
		w, h := f.width, f.height
		if !f.sized {
			w, h = f.sess.window()
		}
		return f, func() tea.Msg { return tea.WindowSizeMsg{Width: w, Height: h} }

	case goneMsg:
		if !f.linger {
			return f, tea.Quit
		}
		f.ended = true
		return f, nil

	case paneMsg:
		// A program that has gone full-screen has no history to be reading;
		// a viewer left looking back at it would be looking at the main
		// screen's past under the alternate screen's present.
		if f.scrolled && f.sess.altScreen() {
			f.scrolled = false
		}
		return f, nil

	case tea.MouseWheelMsg:
		// A selection is in view coordinates and the wheel moves the view.
		f.selected, f.selecting = false, false
		return f.wheeled(msg), nil

	case tea.MouseClickMsg:
		return f.pressed(msg), nil

	case tea.MouseMotionMsg:
		if f.selecting {
			f.sel.head = f.onPane(msg.X, msg.Y)
		}
		return f, nil

	case tea.MouseReleaseMsg:
		return f.released(msg)

	case tea.PasteMsg:
		// Pasting is being present, the same as typing below.
		f.scrolled, f.selected = false, false
		// A paste is one message, not a burst of keystrokes: the frame's own
		// renderer turns bracketed paste on in the viewer's terminal, so the
		// browser wraps what was pasted and the decoder hands it over whole.
		// Dropped rather than forwarded, this is text that simply vanishes.
		//
		// It reaches the container as a paste too, so an app that asked to be
		// told the difference still is.
		f.sess.paste(msg.Content)
		return f, nil

	case tea.ClipboardMsg:
		// A viewer's terminal answered an OSC 52 read the container made. The
		// session decides whether it was asked for; the frame only carries it.
		f.sess.clipboard(msg.Selection, msg.Content)
		return f, nil

	case tea.KeyPressMsg:
		// A frame that outlived its run is waiting for exactly this: any key
		// is the reader saying they have seen the last screen.
		if f.ended {
			return f, tea.Quit
		}
		// Typing is being present. The first keystroke returns a viewer who
		// scrolled back to the live screen, and still goes where it was
		// going, so what the key did is what they see next. A selection is
		// spent the same way: it was copied when the button came up.
		f.scrolled, f.selected = false, false
		// The log view is the frame's, so every key belongs to it: escape
		// leaves, and anything else is somebody reading rather than typing at
		// a terminal they cannot see.
		if f.logs {
			if k := tea.Key(msg); k.Code == tea.KeyEscape || (k.Code == 'l' && k.Mod == tea.ModCtrl) {
				f.logs = false
			}
			return f, nil
		}
		// The code is the frame's the same way: somebody holding a phone up
		// to it is not typing, and a key that reached the program would move
		// the screen under the camera.
		if f.qr {
			if k := tea.Key(msg); k.Code == tea.KeyEscape {
				f.qr = false
			}
			return f, nil
		}
		if f.command {
			return f.commanded(tea.Key(msg))
		}
		// The two keystrokes most likely to end a terminal nobody can reopen,
		// held until they are asked for twice.
		//
		// Most of the time they end nothing: Ctrl-C at a shell prompt clears
		// the line, and Ctrl-D closes a nested shell somebody meant to leave.
		// Which of those it is, is not knowable from here — the same byte
		// exits the session and edits a command line — so the guard is on the
		// keystroke that might, not on one that will.
		//
		// Only where nothing can come back. On a program origin these go
		// straight through: the origin is a path, the program can be run
		// again, and a terminal that argues with Ctrl-C is not a terminal. On
		// a container, PID 1 exiting is the end of it for everybody watching,
		// and one press is a low bar for something with no way back.
		//
		// The first press is not swallowed silently — the border says which
		// key is waiting and that pressing it again sends it — because a key
		// that appears to do nothing reads as a key that is broken.
		if k := tea.Key(msg); k.Mod == tea.ModCtrl && (k.Code == 'c' || k.Code == 'd') && !f.sess.recoverable() {
			if f.armed != k.Code {
				f.armed = k.Code
				f.arming++
				arming := f.arming
				return f, tea.Tick(armGrace, func(time.Time) tea.Msg {
					return disarmMsg{arming: arming}
				})
			}
			f.armed = 0
		}
		// Anything else spends the arming: somebody who typed on is no longer
		// answering the question the border asked.
		if k := tea.Key(msg); f.armed != 0 && f.armed != k.Code {
			f.armed = 0
		}

		// The frame's own key, and the only one it keeps. Ctrl+K on every
		// platform: the page has to be in the way regardless, since the
		// browser claims that chord for its address bar, but the byte that
		// arrives is the one a terminal sends — so this is a rule about a key
		// rather than about a private signal between the two halves.
		//
		// It costs the program kill-to-end-of-line, which is a real key and a
		// cheaper one than Ctrl-D. That reaches the program now: it is a
		// shared session's most dangerous keystroke and no longer this frame's
		// to hold, and `q` below is how somebody ends one on purpose.
		if k := tea.Key(msg); k.Code == 'k' && k.Mod == tea.ModCtrl {
			f.command = true
			return f, nil
		}
		// Anything else is the container's.
		f.sess.sendKey(tea.Key(msg))
		return f, nil
	}
	return f, nil
}

// wheeled decides whose a wheel notch is and delivers it. Three rules, in
// order: a program that asked for the mouse gets it as a mouse event; a
// program on the alternate screen has nothing to scroll back through and gets
// it the way a terminal with alternate scroll would send it, as an arrow key;
// otherwise it is the frame's, and moves this viewer through the history.
//
// The page sends one message per line rather than one per event, so a notch
// here is a line, and the frame does no arithmetic on how far a wheel turned.
func (f frame) wheeled(m tea.MouseWheelMsg) frame {
	if f.logs || f.qr || f.command {
		return f // the frame's own views; nothing to scroll and nobody to tell
	}
	var step int
	switch m.Button {
	case tea.MouseWheelUp:
		step = -1
	case tea.MouseWheelDown:
		step = 1
	default:
		return f // sideways is nobody's
	}

	switch {
	case f.sess.mouseWanted():
		// In the program's coordinates: the frame's border is not its
		// screen, and a window shrunk under the box has less of the screen
		// than the program can see.
		pane := f.pane()
		w, h := f.sess.paneSize()
		f.sess.sendWheel(uv.Mouse{
			X:      max(0, min(m.X-pane.Min.X, w-1)),
			Y:      max(0, min(m.Y-pane.Min.Y, h-1)),
			Button: m.Button,
			Mod:    m.Mod,
		})
		return f
	case f.sess.altScreen():
		code := tea.KeyUp
		if step > 0 {
			code = tea.KeyDown
		}
		f.sess.sendKey(tea.Key{Code: code})
		return f
	}

	kept := f.sess.history()
	if !f.scrolled {
		f.top = kept
	}
	f.top = max(0, min(f.top+step, kept))
	f.scrolled = f.top < kept
	return f
}

// pressed starts a selection where the left button went down, if that is on
// the pane. Anywhere else — the border, the margin outside the box — only
// clears the one there was. Other buttons are nobody's: a right click has no
// menu here and a middle click no paste.
func (f frame) pressed(m tea.MouseClickMsg) frame {
	if m.Button != tea.MouseLeft {
		return f
	}
	f.selected, f.selecting = false, false
	if !uv.Pos(m.X, m.Y).In(f.pane()) {
		return f
	}
	pos := f.onPane(m.X, m.Y)
	f.sel = selection{anchor: pos, head: pos}
	f.selecting = true
	return f
}

// released finishes a selection and copies it. The copy is OSC 52 to this
// viewer's terminal, which is the only clipboard a frame on a console can
// reach; a terminal that does not honour it leaves the selection drawn and
// the text where it was. A press with no drag under it selects nothing and
// copies nothing, so a click is still just a click.
func (f frame) released(m tea.MouseReleaseMsg) (tea.Model, tea.Cmd) {
	if !f.selecting {
		return f, nil
	}
	f.selecting = false
	f.sel.head = f.onPane(m.X, m.Y)
	if f.sel.empty() {
		return f, nil
	}
	f.selected = true
	text := f.sel.text(f.composed())
	if text == "" {
		return f, nil
	}
	return f, tea.SetClipboard(text)
}

// onPane translates a window position into pane coordinates, clamped to the
// pane — a drag that leaves the box selects to its edge rather than past it.
func (f frame) onPane(x, y int) uv.Position {
	pane := f.pane()
	return uv.Pos(
		max(0, min(x-pane.Min.X, pane.Dx()-1)),
		max(0, min(y-pane.Min.Y, pane.Dy()-1)),
	)
}

// behind is how many lines below the pane's last row the live screen's last
// row is: the distance a scrolled viewer would travel to be present again.
// Zero when they are.
func (f frame) behind() int {
	if !f.scrolled {
		return 0
	}
	return max(0, f.sess.history()-f.top)
}

// commanded handles the keystroke after Ctrl-D and leaves command mode, which
// every path does: a mode a viewer can be left in without noticing is worse
// than one that needs the prefix again.
func (f frame) commanded(k tea.Key) (tea.Model, tea.Cmd) {
	f.command = false

	switch k.Code {
	case 'd':
		// This viewer only. The stream is shared and stays up; the socket
		// closing is all that happens, and the page says "detached".
		return f, tea.Quit
	case 'l':
		// The lines tunneld writes about itself, which a viewer has no other
		// way to see: they go to the console this process was started on, and
		// that is somewhere else — or, on a mirrored console, underneath this
		// very frame.
		f.logs = true
		return f, nil
	case 'q':
		// The address as a QR code, for the one reader that cannot click it:
		// a phone pointed at the screen.
		f.qr = true
		return f, nil
	case 'r':
		// The program, started over, for every viewer at once — where the
		// program can be: a container has no PID 1 to start again, and the
		// key is not offered there. Off this goroutine, because ending a
		// program takes as long as it takes to leave.
		if f.sess.restartable() {
			go f.sess.restart()
		}
		return f, nil
	case 'x':
		// The whole run, not this viewer and not this origin: the command ends,
		// its context goes with it, and everything it started — the programs,
		// the attach servers, the tunnel — comes down together. Somebody who
		// opened a terminal from their own machine has no other way to close
		// it from inside, which is the point.
		f.sess.endRun()
		return f, tea.Quit
	}
	// Escape, or anything unbound: the mode closes and the keystroke is spent
	// on closing it. Not forwarded to the container, because a viewer who
	// mistyped a command did not mean to type it at the prompt either.
	return f, nil
}

// box is the frame's outline in this viewer's window: the shared screen with
// the chrome around it and the bar on top, or the window itself when that is
// smaller.
//
// Around the screen rather than around the window, because the screen is the
// smallest viewer's and this window may be larger. A border at the window's
// edges puts the margin inside it, where it reads as the program stopping
// short; a border at the screen's edges puts the margin outside, where it
// reads as what it is — a box the size of somebody else's terminal. The corner
// chip that says whose then sits at the corner of the screen it describes.
//
// Centred in the window. The price is that a renegotiation moves all four
// edges — half the difference each way — where a top-left box would move one;
// the return is a screen that sits where a reader's eye already is.
//
// The bar is part of the box rather than of the window. The rows it takes are
// the same count in every viewer, so the pane is already that much shorter
// everywhere; putting them on the box is what puts the notice at the same
// place relative to the screen everywhere too, instead of a screen-height
// above it in a window much larger than the smallest.
//
// Embedded in the panel, the box is the window. The panel lays a frame of its
// own around every origin that has none, on this terminal's own grid of rows
// and columns, and those frames fill their tiles; a box the size of the
// smallest viewer's screen would stop a row or two short of its neighbours
// and read as the odd one out. So an embedded viewer's border runs to its
// edges and the screen sits inside it, top left, with the room left over
// dark — the corner chip still says whose size the screen is.
func (f frame) box() uv.Rectangle {
	if f.embedded {
		return uv.Rect(0, 0, f.width, f.height)
	}
	banner := f.barRows()
	w, h := f.sess.paneSize()
	w, h = min(f.width, w+chromeWidth), min(f.height, h+chromeHeight+banner)
	return uv.Rect((f.width-w)/2, (f.height-h)/2, w, h)
}

// barRows is how many rows this frame's own bar takes: the session's count, or
// none for a viewer embedded in the panel, whose bar is the panel's. The pane
// is sized for the session's count either way — it is one screen shared by
// every viewer — so an embedded box is that many rows shorter than its window
// and centres in it like any box smaller than its window does.
func (f frame) barRows() int {
	if f.embedded {
		return 0
	}
	return f.sess.bannerRows()
}

// frameRect is the box less its bar: the rectangle the border is drawn
// around, and the one the labels in the border are measured against.
//
// The border goes around the screen, not around the notice. The bar sits on
// top of it the way a title bar sits on a window — the same width, moving
// with it — so everything that is about the border reads this rather than the
// box, and the box stays the answer only to where the whole thing is.
func (f frame) frameRect() uv.Rectangle {
	box := f.box()
	banner := min(f.barRows(), box.Dy())
	return uv.Rect(box.Min.X, box.Min.Y+banner, box.Dx(), box.Dy()-banner)
}

// pane is the area inside the border, in this viewer's window.
func (f frame) pane() uv.Rectangle {
	rect := f.frameRect()
	return uv.Rect(rect.Min.X+1, rect.Min.Y+1, rect.Dx()-chromeWidth, rect.Dy()-chromeHeight)
}

// View draws the container's screen inside a border, with what the session is
// written into the border itself.
//
// Composed into a buffer rather than assembled as text. Both the border and
// the emulator know how to draw themselves into one, so the frame never counts
// a column: no padding a styled line to a width, no measuring around escape
// sequences, and a wide character occupies what it actually occupies.
//
// The cursor is the frame's second job. The pane's own cursor position is
// where the app inside believes it is, and a frame that did not place it there
// would leave every visitor's cursor in the wrong cell — the same disagreement
// between an app and the screen that the emulator exists to prevent. It is
// withheld while a command is pending or the program asked for no cursor,
// where the live position means nothing.
func (f frame) View() tea.View {
	view := tea.NewView("")
	view.AltScreen = true
	view.WindowTitle = f.pageTitle()
	// Clicks, drags and the wheel, reported in SGR, from whatever this frame
	// is drawn on: a real terminal and xterm in a tab both report the mouse
	// once asked, and both stop selecting when they do. The wheel is what
	// makes scrollback reachable; the clicks and drags are what the frame
	// draws its own selection from, which is what gives selecting back —
	// the same way on both, in stream order, copied on release.
	view.MouseMode = tea.MouseModeCellMotion

	// A ScreenBuffer rather than a plain Buffer: it is the one that carries a
	// width method, which is what makes a wide character occupy two columns
	// here the way it does on the terminal this is drawn for.
	// The buffer is the window, so that what is outside the box is drawn as
	// nothing rather than left as whatever was there; the border is the box.
	buf := uv.NewScreenBuffer(f.width, f.height)
	border := uv.RoundedBorder().Style(borderStyle)
	border.Draw(buf, f.frameRect())

	// The provider's word, on top of the border in every view: one row per
	// message, centred on the box, and no key touches it.
	f.drawBanner(buf)

	// No room to frame a screen. The border and the banner are what there
	// is; the pane, its labels and the cursor wait for a window that fits
	// them, rather than being drawn over the rows the box has.
	pane := f.pane()
	if pane.Dx() <= 0 || pane.Dy() <= 0 {
		view.Content = buf.Render()
		return view
	}

	// Filled at its own size and copied in, never drawn straight into the
	// frame's buffer. Neither the emulator nor a styled line clips to the area
	// it is handed — both clip to the screen — so given the whole window they
	// would paint over the border and out of the frame whenever the pane and
	// the emulator disagree about size. They disagree on the ordinary path: a
	// window that has just shrunk draws once before the resize it asked for
	// has come back.
	pixels := f.composed()
	if f.selecting || f.selected {
		f.sel.highlight(pixels)
	}
	blit(buf, pixels, pane.Min.X, pane.Min.Y)

	// The top says what is being watched and where it is being served from,
	// the bottom what to press and what the session is doing. Each pair gives
	// the right-hand label up rather than overlapping the left one when a
	// narrow window cannot hold both.
	// The address is worked out first: it has the top row's other corner, and
	// what is left after it is what the origin has to fit in.
	where := f.where()
	f.topRow(buf, f.frameRect().Min.Y, f.titleLabel(where), f.subtitleLabel(), where)

	// The counts give way whole on a narrow window, but not the one part of
	// them that says this screen is not live: a scrolled viewer with no
	// indicator is a viewer who thinks the program has stopped.
	f.row(buf, f.frameRect().Max.Y-1, f.hint(), f.banner(), f.meta(), f.copied()+f.back())

	view.Content = buf.Render()
	// No cursor when the frame owns the keyboard, when the reader has scrolled
	// off the live screen, or when the program asked for none. The last is
	// what a full-screen program does at startup, and the emulator keeps a
	// position regardless — so drawing one there follows the program's writes
	// around the screen rather than showing anybody where they are typing.
	if !f.command && !f.logs && !f.qr && !f.scrolled && !f.ended && !f.sess.cursorHidden() {
		pos := f.sess.paneCursor()
		if pos.X < pane.Dx() && pos.Y < pane.Dy() {
			view.Cursor = tea.NewCursor(pane.Min.X+pos.X, pane.Min.Y+pos.Y)
		}
	}
	return view
}

// drawBanner draws the provider's word into buf: one row per message across
// the top of the box, centred on it, and no key touches it.
//
// The bar is the box's title bar. It travels with the box, so every viewer
// sees the same notice at the same place relative to the screen — the console
// and a browser tab alike — and a small box in a large window does not carry
// a strip a screen-height away from it, over columns the box does not have.
//
// Each row is filled across the box in the message's own severity colour, the
// same bar the panel's strip draws rather than the frame's own chrome — read
// off a cell the centred text already landed on rather than carried
// separately, so a message with no severity leaves the row exactly as it was
// drawn. An island of colour in the middle of the row reads as a chip;
// spanning the box is what makes it a notice.
//
// An embedded viewer draws none: the panel around it is showing the same
// messages already. See embedded.
func (f frame) drawBanner(buf uv.ScreenBuffer) {
	if f.embedded || f.sess.motd == nil {
		return
	}
	box := f.box()
	for i, line := range f.sess.motd.Lines(box.Dx()) {
		y := box.Min.Y + i
		if y >= f.height {
			break
		}
		width := ansi.StringWidth(line)
		x := box.Min.X + max(0, (box.Dx()-width)/2)
		// Bounded by the box's right edge, so a message wider than the box
		// is cut there rather than running on over the window's margin.
		uv.NewStyledString(line).Draw(buf, uv.Rect(x, y, box.Max.X-x, 1))

		cell := buf.CellAt(x, y)
		if cell == nil || cell.Style.Bg == nil {
			continue
		}
		for cx := box.Min.X; cx < box.Max.X; cx++ {
			if cx >= x && cx < x+width {
				continue
			}
			fill := uv.EmptyCell
			fill.Style.Bg = cell.Style.Bg
			buf.SetCell(cx, y, &fill)
		}
	}
}

// drawQR draws the public address as a code in the pane, centred, with the
// address itself under it for the reader who would rather type. A pane that
// cannot hold the code gets the address and a line saying why, rather than a
// code with its edges cut off — a partial code is not a smaller one, it is not
// a code.
func (f frame) drawQR(pixels uv.ScreenBuffer) {
	w, h := pixels.Bounds().Dx(), pixels.Bounds().Dy()
	centred := func(y int, s string, style uv.Style) {
		width := uv.NewStyledString(s).UnicodeWidth()
		x := max(0, (w-width)/2)
		uv.NewStyledString(style.Styled(s)).Draw(pixels, uv.Rect(x, y, w-x, 1))
	}

	addr := f.sess.announced()
	if addr == "" {
		centred(h/2, "no address yet", logStyle)
		return
	}
	lines, err := qrLines(addr)
	if err != nil {
		centred(h/2-1, "the address could not be encoded: "+err.Error(), logStyle)
		centred(h/2+1, addr, qrStyle)
		return
	}
	// The code, a blank row, and the address.
	if need := len(lines) + 2; need > h || len([]rune(lines[0])) > w {
		centred(h/2-1, fmt.Sprintf("the pane is too small for a code: %d×%d needed", len([]rune(lines[0])), need), logStyle)
		centred(h/2+1, addr, qrStyle)
		return
	}
	top := (h - (len(lines) + 2)) / 2
	for i, line := range lines {
		centred(top+i, line, qrStyle)
	}
	centred(top+len(lines)+1, addr, qrStyle)
}

// composed is the pane as this viewer sees it, at the pane's own size: the
// logs, the code, history from this viewer's top, or the live screen. What
// View blits into the frame, and what a selection reads its text from — the
// same cells either way, so what is copied is what was highlighted.
func (f frame) composed() uv.ScreenBuffer {
	pane := f.pane()
	pixels := uv.NewScreenBuffer(max(0, pane.Dx()), max(0, pane.Dy()))
	switch {
	case pane.Dx() <= 0 || pane.Dy() <= 0:
	case f.logs:
		// The last lines that fit, because what somebody opening this wants is
		// what just happened rather than what happened first.
		for i, l := range lastOf(f.sess.logLines(), pane.Dy()) {
			uv.NewStyledString(logStyle.Styled(l)).Draw(pixels, uv.Rect(0, i, pane.Dx(), 1))
		}
	case f.qr:
		f.drawQR(pixels)
	case f.scrolled:
		// History from this viewer's top, and the live screen under it.
		f.sess.drawHistory(pixels, pixels.Bounds(), f.top)
	default:
		// The emulator draws itself, cell for cell, with nothing re-parsed on
		// the way.
		f.sess.drawPane(pixels, pixels.Bounds())
	}
	return pixels
}

// lastOf is the tail of lines that fits in rows, and a line saying so when
// there is nothing to show. An empty pane would read as a frame that had
// broken rather than a process that has been quiet.
func lastOf(lines []string, rows int) []string {
	if rows <= 0 {
		return nil
	}
	if len(lines) == 0 {
		return []string{"nothing logged yet — tunneld says more at --log-level debug"}
	}
	if len(lines) > rows {
		lines = lines[len(lines)-rows:]
	}
	return lines
}

// blit copies src into dst with its top-left corner at x, y.
func blit(dst, src uv.ScreenBuffer, x, y int) {
	for row := range src.Bounds().Dy() {
		for col := range src.Bounds().Dx() {
			dst.SetCell(x+col, y+row, src.CellAt(col, row))
		}
	}
}

// row draws a border row's labels: left from the indent, right against the far
// corner, and centre in the frame if what is left of the row can hold it.
//
// The left label keeps its columns and the right one gives way, which is the
// bottom border's rule: what to press has to be legible, and half a label
// pushed into another reads as neither. The centre goes first of the three
// because it is the least urgent, and it is centred on the frame rather than
// in the gap so that it stays put as the counts beside it change width.
//
// The right label is given as candidates, most complete first, and the first
// that fits is the one drawn — so a label that is mostly optional detail can
// fall back to the part of itself that is not, rather than vanishing whole.
func (f frame) row(buf uv.ScreenBuffer, y int, left, centre string, right ...string) {
	box := f.frameRect()
	indent, edge := box.Min.X+2, box.Max.X-1

	after := writeAt(buf, indent, y, left, edge-indent)

	before := edge
	for _, label := range right {
		width := uv.NewStyledString(label).UnicodeWidth()
		if width == 0 {
			continue
		}
		if x := edge - width; x > after {
			writeAt(buf, x, y, label, edge-x)
			before = x
			break
		}
	}
	f.between(buf, y, centre, after, before)
}

// topRow is row with the two ends' priority reversed: the right label is
// placed first and the left takes what is left over.
//
// The top border's two corners are the origin and the address, and they are
// not worth the same. The origin is a string somebody typed, and the frame
// says it again as the page's own title; the address is the tunnel's, minted
// for this run, and appears nowhere else on the screen. So when the row cannot
// hold both, the one that can be recovered is the one that goes.
//
// The address still gives way below the width where it fits the row at all —
// there is nothing to trade against, and an address clipped to a corner is not
// an address. The origin has the row to itself again there.
func (f frame) topRow(buf uv.ScreenBuffer, y int, left, centre, right string) {
	box := f.frameRect()
	indent, edge := box.Min.X+2, box.Max.X-1

	before := edge
	if width := uv.NewStyledString(right).UnicodeWidth(); width > 0 {
		if x := edge - width; x > indent {
			writeAt(buf, x, y, right, edge-x)
			before = x
		}
	}

	// The whole row when the address was not drawn, and a column short of it
	// when it was — the same gap row leaves between the two.
	room := edge - indent
	if before < edge {
		room = before - indent - 1
	}
	after := writeAt(buf, indent, y, left, room)

	f.between(buf, y, centre, after, before)
}

// between centres centre on the frame, if the columns left between the two end
// labels can hold it.
func (f frame) between(buf uv.ScreenBuffer, y int, centre string, after, before int) {
	if width := uv.NewStyledString(centre).UnicodeWidth(); width > 0 {
		box := f.frameRect()
		if x := box.Min.X + (box.Dx()-width)/2; x > after && x+width < before {
			writeAt(buf, x, y, centre, before-x)
		}
	}
}

// writeAt draws a styled run at x on row y, clipped to width columns, and
// reports the column after the last one it used.
//
// Drawn into a buffer of its own and copied in, for the same reason the pane
// is: a run drawn straight into the frame's buffer clears what it does not
// cover and clips only at the screen, so a label shorter than its area would
// erase the border to its right and a longer one would erase the corner. Sized
// here, it truncates instead — a long container name loses its tail and the
// box stays a box.
func writeAt(buf uv.ScreenBuffer, x, y int, s string, width int) int {
	if width <= 0 {
		return x
	}
	label := uv.NewStyledString(s)
	used := min(label.UnicodeWidth(), width)
	if used <= 0 {
		return x
	}

	cells := uv.NewScreenBuffer(used, 1)
	label.Draw(cells, cells.Bounds())
	blit(buf, cells, x, y)
	return x + used
}

// titleLabel is what is being watched, in the top border: the origin, written
// the way it was typed. The scheme stays on rather than being trimmed to the
// container — it is what says this is a container at all, and the same string
// pasted back into a command line is a working origin.
//
// The origin comes whole from the Target rather than being assembled here: a
// provider knows its own verb and authority, so a program frames itself as
// exec:///usr/bin/htop where a container frames itself as attach://dockerd/api.
func (f frame) titleLabel(beside string) string {
	origin := f.title()

	// The row to itself where the address is not being drawn at all, which is
	// the one case the origin is not competing with anything.
	if !f.drawn(beside) {
		return nameStyle.Styled(" " + shorten(origin, f.titleRoom("")) + " ")
	}

	room := f.titleRoom(beside)
	short := shorten(origin, room)
	if ansi.StringWidth(short) > room {
		// Narrower than the shortest thing the origin can honestly be reduced
		// to. It goes rather than being clipped: topRow says why the address
		// is the one kept, and a stub reading exec:///Us tells nobody
		// anything the frame does not say better in the page's title.
		return ""
	}
	return nameStyle.Styled(" " + short + " ")
}

// drawn reports whether the row will place beside against its corner. Below
// this it does not fit the row on its own, and topRow drops it.
func (f frame) drawn(beside string) bool {
	width := uv.NewStyledString(beside).UnicodeWidth()
	return width > 0 && width < f.frameRect().Dx()-3
}

// titleRoom is how many columns the origin can have without costing the label
// against the other corner.
//
// Read off topRow: the left label starts two columns into the box, the right
// one ends at the box's last column, and a column is left between them. The label's own spaces are the
// last two.
func (f frame) titleRoom(beside string) int {
	if width := uv.NewStyledString(beside).UnicodeWidth(); width > 0 {
		return f.frameRect().Dx() - 1 - width - 1 - 2 - 2
	}
	return f.frameRect().Dx() - 1 - 2 - 2
}

// ellipsis stands for the leading path segments an origin has given up.
const ellipsis = "..."

// shorten is origin with as few leading path segments as the width demands
// replaced by an ellipsis, and origin itself when that cannot help.
//
// Which end goes is the whole of this: an origin truncated the way a label is,
// from the tail, loses exactly the part that says which program is running and
// keeps the part that says whose home directory it is under. So the head is
// spent instead, one segment at a time and no more than the width asks for.
//
// The scheme and the authority always stay — they are what makes the string an
// origin rather than a path — and so does the last segment, which is the name.
// An origin with nothing between the two comes back untouched: a container's
// single path segment is its name, and dropping it would leave attach://dockerd
// standing for a container. The caller's writeAt then clips it as it always
// has, so a window too narrow for any of this still draws a frame.
func shorten(origin string, width int) string {
	if width <= 0 || ansi.StringWidth(origin) <= width {
		return origin
	}

	scheme := strings.Index(origin, "://")
	if scheme < 0 {
		return origin
	}
	// The first separator after the authority, which the prefix keeps: the
	// path is what comes after it, and on a program origin the authority is
	// empty, so that separator is the root.
	root := strings.Index(origin[scheme+len("://"):], "/")
	if root < 0 {
		return origin
	}

	at := scheme + len("://") + root + 1
	prefix, segments := origin[:at], strings.Split(origin[at:], "/")
	for i := 1; i < len(segments); i++ {
		if short := prefix + ellipsis + "/" + strings.Join(segments[i:], "/"); ansi.StringWidth(short) <= width {
			return short
		}
	}
	return origin
}

// title is the origin, unstyled, for the places that cannot carry styling.
func (f frame) title() string {
	return f.sess.Origin()
}

// subtitleLabel is what the terminal says it is doing, centred along the top.
//
// Both of the names it goes by, because they are not the same thing: a prompt
// framework sets the title to the running command's whole line and the
// subtitle to its name, and at rest one is user@host:directory and the other
// the directory alone. Joined when they differ and said once when they do not,
// which is what an app setting both with a single OSC 0 leaves.
//
// Shown as they arrive rather than interpreted — nothing distinguishes "a
// command is running" from "this is the prompt", and a guess about which is
// which would be a guess about somebody's shell configuration.
//
// Blank on a terminal that names itself neither way, which is most shells
// without a prompt framework. That is the honest answer and costs the row
// nothing.
func (f frame) subtitleLabel() string {
	subtitle := f.subtitle()
	if subtitle == "" {
		return ""
	}
	return titleStyle.Styled(" " + subtitle + " ")
}

// subtitle is the two names the terminal goes by, joined: its own title, and
// its subtitle after that when it adds anything. One of them when they are the
// same, which is what an app setting both with a single OSC 0 leaves.
//
// Both are reduced to what can be shown before anything measures them — see
// showable.
func (f frame) subtitle() string {
	title, subtitle := f.sess.titles()
	title, subtitle = showable(title), showable(subtitle)
	switch {
	case subtitle == "" || subtitle == title:
		return title
	case title == "":
		return subtitle
	default:
		return title + " · " + subtitle
	}
}

// pageTitle is what the browser tab displaying this terminal is called: the
// origin, and whatever the terminal is calling itself.
//
// Carried as the frame's own window title, which the renderer emits as an OSC
// and the page raises to document.title. So the mechanism an app uses to name
// its terminal is the one that ends up naming the tab it is displayed in,
// which is what it was for.
//
// The same pair the border shows, so a tab and the frame inside it agree about
// what is running.
//
// What the terminal is doing goes first and the origin after it, because a
// browser tab is narrow and loses its end: a row of them all beginning with
// the same attach://dockerd/ would be a row that says nothing. A terminal that has
// not named itself leaves the origin on its own rather than a word standing in
// for one — there is nothing to say, and saying so is not better.
func (f frame) pageTitle() string {
	if subtitle := f.subtitle(); subtitle != "" {
		return subtitle + " · " + f.title()
	}
	return f.title()
}

// showable reduces a title to what will actually appear, before anything
// measures it.
//
// A title is whatever an app decided to put there, and things that take up
// columns without drawing in them — control characters, zero-width joiners —
// are counted when the label is measured and blank when it is painted. Since
// the label is written over the border, the difference is a hole in the top of
// the box.
func showable(title string) string {
	return strings.TrimSpace(strings.Map(printable, strings.ToValidUTF8(title, "")))
}

// printable drops a rune that would occupy a cell without filling it.
func printable(r rune) rune {
	if unicode.IsPrint(r) {
		return r
	}
	return -1
}

// where is the public address this origin answers on, in the top right, once
// the tunnel has one to give.
//
// Opposite the origin, which is where the container is reached from inside:
// the two corners of the top border are the two ends of the same thing. It is
// the tunnel's own address rather than the one this viewer happened to type,
// which is what makes it worth showing — it is the address to send somebody
// else, and for one origin among several it carries the routing parameter that
// reaches this one.
//
// Embedded in the panel, the corner holds a chip instead: the panel's own
// frames carry their controls there as chips, and the address in full is
// the panel's to show, once, in its tab. The chip is the same hyperlink, so
// a click on it opens the origin in a tab of its own — the panel's popout,
// drawn by the terminal.
func (f frame) where() string {
	addr := f.sess.announced()
	if addr == "" {
		return ""
	}
	// Marked as a hyperlink as well as printed. A terminal that understands
	// OSC 8 makes it clickable; one that does not drops the escape and is left
	// with exactly the text it would have had, which is why it costs nothing
	// to send. It occupies no columns either, so the alignment either side of
	// it is unaffected.
	if f.embedded {
		return " " + ansi.SetHyperlink(addr) + chipStyle.Styled(" ↗ ") + ansi.ResetHyperlink() + " "
	}
	return addrStyle.Styled(" " + ansi.SetHyperlink(addr) + addr + ansi.ResetHyperlink() + " ")
}

// banner is the build this is running, along the bottom. It is what a bug
// report needs and nobody thinks to ask for, so it sits where it can be read
// without being in the way.
func (f frame) banner() string {
	if f.sess.banner == "" {
		return ""
	}
	return bannerStyle.Styled(" " + f.sess.banner + " ")
}

// meta is what the session is doing, in the shape k9s writes one: where it is
// being served from, what it is showing, and how big it is.
//
// The host leads, because it is the one part of the frame that is not a name
// somebody chose. Through a tunnel the page could be open from anywhere, and
// the origin in the top corner is a container reference an operator typed —
// neither says which machine is actually serving this, which is the question
// somebody with two of these open has.
func (f frame) meta() string {
	qualifier := viewers(f.sess.count())
	w, h := f.sess.paneSize()

	var where string
	if host() != "" {
		where = hostStyle.Styled(" " + host())
	}
	// How far back this viewer is reading, where the viewer count goes: it is
	// the one thing about the frame that is this viewer's alone.
	return f.copied() + f.back() + where +
		qualStyle.Styled(" ("+qualifier+")") +
		countStyle.Styled(fmt.Sprintf(" [%d×%d] ", w, h))
}

// back is the chip saying how far behind live this viewer is reading, or
// nothing when they are present. On its own as well as inside meta, because
// it is the part of the counts a narrow window must not drop: a screen that
// has stopped following the program needs to say so.
func (f frame) back() string {
	if n := f.behind(); n > 0 {
		return backStyle.Styled(fmt.Sprintf(" ↑%d ", n))
	}
	return ""
}

// copied is the chip saying the highlighted text went to the clipboard, or
// nothing when nothing is selected. The only acknowledgement a terminal gives
// for OSC 52 is none, so this is it.
func (f frame) copied() string {
	if f.selected {
		return copyStyle.Styled(" copied ")
	}
	return ""
}

// hint is the keys, in the bottom border: the one that opens the commands, or
// the commands themselves once it has.
func (f frame) hint() string {
	if f.ended {
		return chipStyle.Styled(" ended ") + hintStyle.Styled(" any key to leave ")
	}
	if f.armed != 0 {
		// Which key, and what to do about it. Nothing about what it will do:
		// Ctrl-C at a shell prompt clears the line and nothing else, and a
		// frame warning of an ending at the most ordinary keystroke there is
		// would be worth nothing by the third time somebody saw it.
		return chipStyle.Styled(" ^"+strings.ToUpper(string(f.armed))+" ") +
			hintStyle.Styled(" again to send it ")
	}
	if f.logs || f.qr {
		return chipStyle.Styled(" esc ") + hintStyle.Styled(" back to the terminal ")
	}
	if !f.command {
		return chipStyle.Styled(" ^K ") + hintStyle.Styled(" commands ")
	}
	// r only where it works: a program can be started over, a container
	// cannot, and a key that appears to do nothing reads as a key that is
	// broken.
	var restart string
	if f.sess.restartable() {
		restart = chipStyle.Styled(" r ") + hintStyle.Styled(" restart ")
	}
	return chipStyle.Styled(" d ") + hintStyle.Styled(" detach ") +
		chipStyle.Styled(" x ") + hintStyle.Styled(" exit ") +
		restart +
		chipStyle.Styled(" l ") + hintStyle.Styled(" logs ") +
		chipStyle.Styled(" q ") + hintStyle.Styled(" qr ") +
		chipStyle.Styled(" esc ") + hintStyle.Styled(" cancel ")
}

// viewers names how many are watching, in the one place it is said.
func viewers(n int) string {
	if n == 1 {
		return "1 viewer"
	}
	return fmt.Sprintf("%d viewers", n)
}

// asKeyEvent converts a keystroke the frame decoded back into one the emulator
// can encode for the container.
//
// The round trip is exact rather than approximate: bubbletea's Key and
// ultraviolet's carry the same six fields, and bubbletea's key codes are
// declared as ultraviolet's own constants, so nothing is being mapped or
// guessed here. Going through the emulator rather than writing bytes straight
// to stdin is what keeps the encoding correct as the app changes its mind: the
// emulator knows whether the app has asked for application cursor keys, and a
// raw byte written past it would not.
func asKeyEvent(k tea.Key) uv.KeyPressEvent {
	return uv.KeyPressEvent{
		Text:        k.Text,
		Mod:         uv.KeyMod(k.Mod),
		Code:        k.Code,
		ShiftedCode: k.ShiftedCode,
		BaseCode:    k.BaseCode,
		IsRepeat:    k.IsRepeat,
	}
}
