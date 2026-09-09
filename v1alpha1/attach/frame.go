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
)

// How long a frame waits to be told how big its window is before drawing at
// the size the session settled on instead.
//
// The page sends its size the moment its socket opens, so this is only ever
// spent on something that is not the page. Half a second is longer than the
// round trip through the edge and short enough that a client which never sends
// one is not left staring at nothing.
const sizeGrace = 500 * time.Millisecond

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

	// command is Ctrl-D having been pressed: the next keystroke belongs to the
	// frame and the container will not see it.
	command bool

	// sized is the window having been reported by the page rather than assumed.
	// Until it is, the frame draws nothing rather than drawing at a size that
	// is somebody else's.
	sized bool

	// scroll is how many lines back the pane is showing, 0 being live. An
	// altscreen frame has no browser scrollback of its own to fall back on —
	// that is what the frame costs — so the emulator's is reached through
	// here instead.
	scroll int
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
		f.scroll = 0
		f.sess.paste(msg.Content)
		return f, nil

	case tea.KeyPressMsg:
		if f.command {
			return f.commanded(tea.Key(msg))
		}
		if k := tea.Key(msg); k.Code == 'd' && k.Mod == tea.ModCtrl {
			f.command = true
			return f, nil
		}
		// Anything else is the container's. A keystroke that arrives while the
		// pane is scrolled back snaps it live first, the way a terminal does:
		// what was typed is meant for the prompt, and the prompt is at the
		// bottom.
		f.scroll = 0
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
	case 'q':
		// What Ctrl-D would have done if the frame were not holding it: end of
		// file to the shared stdin, which is the shell's cue to exit and the
		// session's to be over for everyone. Deliberate now rather than
		// accidental, which is the whole of the guard.
		f.sess.eof()
		return f, tea.Quit
	case 'k', tea.KeyUp:
		f.scroll = f.sess.scrollUp(f.scroll)
		return f, nil
	case 'j', tea.KeyDown:
		if f.scroll > 0 {
			f.scroll--
		}
		return f, nil
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
// withheld while the pane is scrolled back or a command is pending, where the
// live cursor position means nothing.
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
	if f.scroll == 0 {
		// The emulator draws itself, cell for cell, with nothing re-parsed on
		// the way.
		f.sess.drawPane(pixels, pixels.Bounds())
	} else {
		// Scrolled back is the one thing the emulator will not draw: what is
		// wanted is partly its scrollback, which is lines rather than a
		// screen.
		for i, line := range f.sess.paneLines(f.scroll, pane.Dy()) {
			uv.NewStyledString(line).Draw(pixels, uv.Rect(0, i, pane.Dx(), 1))
		}
	}
	blit(buf, pixels, pane.Min.X, pane.Min.Y)

	// The top says what is being watched and where it is being served from,
	// the bottom what to press and what the session is doing. Each pair gives
	// the right-hand label up rather than overlapping the left one when a
	// narrow window cannot hold both.
	f.row(buf, 0, f.titleLabel(), f.subtitleLabel(), f.where())

	f.row(buf, f.height-1, f.hint(), f.banner(), f.meta())

	view.Content = buf.Render()
	if !f.command && f.scroll == 0 {
		pos := f.sess.paneCursor()
		if pos.X < pane.Dx() && pos.Y < pane.Dy() {
			view.Cursor = tea.NewCursor(pane.Min.X+pos.X, pane.Min.Y+pos.Y)
		}
	}
	return view
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
// They give way in that order. What is on the left is the thing that has to be
// legible — what to press, and what you are attached to — and half a label
// pushed into another reads as neither. The centre goes first because it is
// the least urgent of the three, and it is centred on the frame rather than in
// the gap so that it stays put as the counts beside it change width.
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
// The scheme comes from the Target rather than from a guess here: a provider
// carries its own, so a program frames itself as file://htop where a container
// frames itself as dockerd://api.
func (f frame) titleLabel() string {
	return nameStyle.Styled(" " + f.title() + " ")
}

// title is the origin, unstyled, for the places that cannot carry styling.
func (f frame) title() string {
	return f.sess.Scheme() + "://" + f.sess.Name()
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
// the same dockerd:// would be a row that says nothing. A terminal that has
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
	if f.scroll > 0 {
		qualifier = fmt.Sprintf("scrolled back %d", f.scroll)
	}
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
	if !f.command {
		return chipStyle.Styled(" ^D ") + hintStyle.Styled(" commands ")
	}
	return chipStyle.Styled(" d ") + hintStyle.Styled(" detach ") +
		chipStyle.Styled(" q ") + hintStyle.Styled(" end ") +
		chipStyle.Styled(" k/j ") + hintStyle.Styled(" scroll ") +
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
