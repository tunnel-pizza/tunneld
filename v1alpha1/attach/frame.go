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
	hintStyle   = uv.Style{Fg: ansi.IndexedColor(245)}
	// logStyle is dimmer than the terminal it covers, because what it shows is
	// tunneld talking about itself rather than the thing anybody came to see.
	logStyle = uv.Style{Fg: ansi.IndexedColor(245)}
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

	// The viewer's window, as it last reported it. The pane inside is this
	// less the chrome, and smaller again when another viewer has a smaller
	// window — the session settles the emulator on the smallest of them, and
	// a larger window renders it with unused margin.
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

	// sized is the window having been reported by the page rather than assumed.
	// Until it is, the frame draws nothing rather than drawing at a size that
	// is somebody else's.
	sized bool
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
		return f, tea.Quit

	case paneMsg:
		return f, nil

	case tea.PasteMsg:
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
		// The log view is the frame's, so every key belongs to it: escape
		// leaves, and anything else is somebody reading rather than typing at
		// a terminal they cannot see.
		if f.logs {
			if k := tea.Key(msg); k.Code == tea.KeyEscape || (k.Code == 'l' && k.Mod == tea.ModCtrl) {
				f.logs = false
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

// pane is the area inside the border, in this viewer's window.
func (f frame) pane() uv.Rectangle {
	return uv.Rect(1, 1, f.width-chromeWidth, f.height-chromeHeight)
}

// paneRows is how many rows of the container's screen this viewer can show.
func (f frame) paneRows() int {
	if rows := f.height - chromeHeight; rows > 0 {
		return rows
	}
	return 0
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

	pane := f.pane()
	if pane.Dx() <= 0 || pane.Dy() <= 0 {
		// No room to frame anything. Better an empty screen than a border
		// drawn over the only rows the container had.
		return view
	}

	// A ScreenBuffer rather than a plain Buffer: it is the one that carries a
	// width method, which is what makes a wide character occupy two columns
	// here the way it does on the terminal this is drawn for.
	buf := uv.NewScreenBuffer(f.width, f.height)
	border := uv.RoundedBorder().Style(borderStyle)
	border.Draw(buf, buf.Bounds())

	// Filled at its own size and copied in, never drawn straight into the
	// frame's buffer. Neither the emulator nor a styled line clips to the area
	// it is handed — both clip to the screen — so given the whole window they
	// would paint over the border and out of the frame whenever the pane and
	// the emulator disagree about size. They disagree on the ordinary path: a
	// window that has just shrunk draws once before the resize it asked for
	// has come back.
	pixels := uv.NewScreenBuffer(pane.Dx(), pane.Dy())
	if f.logs {
		// The last lines that fit, because what somebody opening this wants is
		// what just happened rather than what happened first.
		for i, l := range lastOf(f.sess.logLines(), pane.Dy()) {
			uv.NewStyledString(logStyle.Styled(l)).Draw(pixels, uv.Rect(0, i, pane.Dx(), 1))
		}
	} else {
		// The emulator draws itself, cell for cell, with nothing re-parsed on
		// the way.
		f.sess.drawPane(pixels, pixels.Bounds())
	}
	blit(buf, pixels, pane.Min.X, pane.Min.Y)

	// The top says what is being watched and where it is being served from,
	// the bottom what to press and what the session is doing. Each pair gives
	// the right-hand label up rather than overlapping the left one when a
	// narrow window cannot hold both.
	// The address is worked out first: it has the top row's other corner, and
	// what is left after it is what the origin has to fit in.
	where := f.where()
	f.topRow(buf, 0, f.titleLabel(where), f.subtitleLabel(), where)

	f.row(buf, f.height-1, f.hint(), f.banner(), f.meta())

	view.Content = buf.Render()
	// No cursor when the frame owns the keyboard, when the reader has scrolled
	// off the live screen, or when the program asked for none. The last is
	// what a full-screen program does at startup, and the emulator keeps a
	// position regardless — so drawing one there follows the program's writes
	// around the screen rather than showing anybody where they are typing.
	if !f.command && !f.logs && !f.sess.cursorHidden() {
		pos := f.sess.paneCursor()
		if pos.X < pane.Dx() && pos.Y < pane.Dy() {
			view.Cursor = tea.NewCursor(pane.Min.X+pos.X, pane.Min.Y+pos.Y)
		}
	}
	return view
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
func (f frame) row(buf uv.ScreenBuffer, y int, left, centre, right string) {
	const indent = 2
	edge := f.width - 1

	after := writeAt(buf, indent, y, left, edge-indent)

	before := edge
	if width := uv.NewStyledString(right).UnicodeWidth(); width > 0 {
		if x := edge - width; x > after {
			writeAt(buf, x, y, right, edge-x)
			before = x
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
	const indent = 2
	edge := f.width - 1

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
		if x := (f.width - width) / 2; x > after && x+width < before {
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
	return width > 0 && width < f.width-3
}

// titleRoom is how many columns the origin can have without costing the label
// against the other corner.
//
// Read off topRow: the left label starts at column 2, the right one ends at
// f.width-1, and a column is left between them. The label's own spaces are the
// last two.
func (f frame) titleRoom(beside string) int {
	if width := uv.NewStyledString(beside).UnicodeWidth(); width > 0 {
		return f.width - 1 - width - 1 - 2 - 2
	}
	return f.width - 1 - 2 - 2
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
	return where +
		qualStyle.Styled(" ("+qualifier+")") +
		countStyle.Styled(fmt.Sprintf(" [%d×%d] ", w, h))
}

// hint is the keys, in the bottom border: the one that opens the commands, or
// the commands themselves once it has.
func (f frame) hint() string {
	if f.armed != 0 {
		// Which key, and what to do about it. Nothing about what it will do:
		// Ctrl-C at a shell prompt clears the line and nothing else, and a
		// frame warning of an ending at the most ordinary keystroke there is
		// would be worth nothing by the third time somebody saw it.
		return chipStyle.Styled(" ^"+strings.ToUpper(string(f.armed))+" ") +
			hintStyle.Styled(" again to send it ")
	}
	if f.logs {
		return chipStyle.Styled(" esc ") + hintStyle.Styled(" back to the terminal ")
	}
	if !f.command {
		return chipStyle.Styled(" ^K ") + hintStyle.Styled(" commands ")
	}
	return chipStyle.Styled(" d ") + hintStyle.Styled(" detach ") +
		chipStyle.Styled(" x ") + hintStyle.Styled(" exit ") +
		chipStyle.Styled(" l ") + hintStyle.Styled(" logs ") +
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
