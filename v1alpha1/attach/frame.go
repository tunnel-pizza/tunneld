package attach

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	uv "github.com/charmbracelet/ultraviolet"
)

// The rows the frame keeps for itself. One, and it is the status line: the
// terminal page's own rule is that the terminal is the page, because anything
// framing it steals rows from a viewport that is often a phone. A row is what
// it costs to have somewhere for the session to say what it is.
const chromeHeight = 1

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
		// believes it has no rows draws none of them: the viewer watches an
		// empty tab until its page happens to send a size.
		//
		// Answered rather than raced. Pushing the real size in alongside
		// startup means whichever lands second wins, and when that is the zero
		// the frame never draws again. Sent back as a command instead, so it
		// is this message that produces the correct one and ordering has
		// nothing to decide.
		if msg.Width == 0 || msg.Height == 0 {
			w, h := f.sess.window()
			return f, func() tea.Msg { return tea.WindowSizeMsg{Width: w, Height: h} }
		}
		f.width, f.height = msg.Width, msg.Height
		f.sess.resizeViewer(f.v, msg.Width, msg.Height)
		return f, nil

	case goneMsg:
		return f, tea.Quit

	case paneMsg:
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

// paneRows is how many rows of the container's screen this viewer can show.
func (f frame) paneRows() int {
	if rows := f.height - chromeHeight; rows > 0 {
		return rows
	}
	return 0
}

// View draws the pane and the status line under it.
//
// The cursor is the frame's second job. The pane's own cursor position is
// where the app inside believes it is, and a frame that did not place it there
// would leave every visitor's cursor in the wrong cell — the same disagreement
// between an app and the screen that the emulator exists to prevent. It is
// withheld while the pane is scrolled back or a command is pending, where the
// live cursor position means nothing.
func (f frame) View() tea.View {
	rows := f.paneRows()
	lines := f.sess.paneLines(f.scroll, rows)

	var b strings.Builder
	for _, line := range lines {
		b.WriteString(line)
		// Reset at every line end. The emulator renders styling per line and a
		// line that ends inside an SGR run would otherwise colour the status
		// line, or the line under it, with whatever the app left open.
		b.WriteString("\x1b[0m\n")
	}
	b.WriteString(f.status())

	view := tea.NewView(b.String())
	view.AltScreen = true
	if !f.command && f.scroll == 0 {
		pos := f.sess.paneCursor()
		if pos.Y < rows {
			view.Cursor = tea.NewCursor(pos.X, pos.Y)
		}
	}
	return view
}

// status is the one row the frame keeps. It says what the session is when
// there is nothing to do, and what the keys are when there is.
func (f frame) status() string {
	var s string
	switch {
	case f.command:
		s = "^D  d detach · q end session · k/j scroll · esc cancel"
	case f.scroll > 0:
		s = fmt.Sprintf("%s · scrolled back %d · ^D", f.sess.Name(), f.scroll)
	default:
		w, h := f.sess.paneSize()
		s = fmt.Sprintf("%s · %s · %d×%d · ^D", f.sess.Name(), viewers(f.sess.count()), w, h)
	}

	// Truncated to the window rather than wrapped: a status line that wraps
	// costs a second row that the pane was drawn assuming it still had, and
	// the frame would paint over the bottom of the container's screen.
	if f.width > 0 && len(s) > f.width {
		s = s[:f.width]
	}
	// Dim, so the row reads as the frame's and not as something the container
	// printed.
	return "\x1b[2m" + s + "\x1b[0m"
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
