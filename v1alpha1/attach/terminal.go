package attach

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	uv "github.com/charmbracelet/ultraviolet"
)

// Terminal is a served origin's session as an agent reaches it: the program
// a tab shows, typed into and read the way a tab does. It holds the session,
// not a run, so it outlives a restart.
type Terminal interface {
	// Send sends each step of input in order, starting the program again
	// first if it has exited and can be, as a tab arriving does, and answers
	// the cursor the input went in at, which reader's position moves to:
	// what the program prints in reply is read from there. A step it cannot
	// send sends nothing: ErrUnknownKey for a key name it does not know,
	// ErrBadInput for a step that is not exactly one of text or key.
	Send(reader string, input ...Input) (string, error)
	// Read is the program's output from cursor, or from where reader last
	// stopped when cursor is "", as text; reader's position moves to where
	// it stops. A cursor not this terminal's is ErrStaleCursor or
	// ErrBadCursor.
	Read(reader, cursor string) (Output, error)
	// Wait is Read once until matches its text, the program has printed
	// nothing for idle, or ctx is done, whichever is first, and whether until
	// matched. A nil until never matches, and an idle of zero never ends it.
	Wait(ctx context.Context, reader, cursor string, until *regexp.Regexp, idle time.Duration) (Output, bool, error)
	// Screen is what the terminal shows now.
	Screen() Screen
}

// Input is one step of what Send sends: text as typed, or one key by name.
type Input struct {
	Text string `json:"text,omitempty" jsonschema:"text to type, as typed: a newline is not Enter"`
	Key  string `json:"key,omitempty" jsonschema:"a key to press: Enter, Tab, Esc, Backspace, Delete, Space, Up, Down, Left, Right, Home, End, PageUp, PageDown, F1 to F12, or Ctrl-<letter>"`
}

// Screen is a terminal's visible rows as text, trailing blanks dropped,
// with where the cursor is, its title, and whether the program is on the
// alternate screen.
type Screen struct {
	Width     int      `json:"width" jsonschema:"the screen's width in columns, the person's: an agent never sets it"`
	Height    int      `json:"height" jsonschema:"the screen's height in rows"`
	Rows      []string `json:"rows" jsonschema:"the visible rows, top first, trailing blanks dropped"`
	Cursor    Cursor   `json:"cursor" jsonschema:"where the cursor is"`
	AltScreen bool     `json:"alt_screen" jsonschema:"whether a full-screen program (an editor, k9s, htop) is on the alternate screen"`
	Title     string   `json:"title" jsonschema:"the title the program set, empty when none"`
}

// Cursor is a position on a Screen, zero-based.
type Cursor struct {
	Row int `json:"row" jsonschema:"the row, from 0 at the top"`
	Col int `json:"col" jsonschema:"the column, from 0 at the left"`
}

// ErrUnknownKey is Send given a key name it does not know.
var ErrUnknownKey = errors.New("attach: unknown key")

// ErrBadInput is Send given a step with both text and a key, or neither.
var ErrBadInput = errors.New("attach: a step is text or a key")

// keys is every key name Send knows, but Ctrl-<letter>.
var keys = map[string]uv.Key{
	"Enter":     {Code: uv.KeyEnter},
	"Tab":       {Code: uv.KeyTab},
	"Esc":       {Code: uv.KeyEscape},
	"Backspace": {Code: uv.KeyBackspace},
	"Delete":    {Code: uv.KeyDelete},
	"Space":     {Code: uv.KeySpace, Text: " "},
	"Up":        {Code: uv.KeyUp},
	"Down":      {Code: uv.KeyDown},
	"Left":      {Code: uv.KeyLeft},
	"Right":     {Code: uv.KeyRight},
	"Home":      {Code: uv.KeyHome},
	"End":       {Code: uv.KeyEnd},
	"PageUp":    {Code: uv.KeyPgUp},
	"PageDown":  {Code: uv.KeyPgDown},
	"F1":        {Code: uv.KeyF1},
	"F2":        {Code: uv.KeyF2},
	"F3":        {Code: uv.KeyF3},
	"F4":        {Code: uv.KeyF4},
	"F5":        {Code: uv.KeyF5},
	"F6":        {Code: uv.KeyF6},
	"F7":        {Code: uv.KeyF7},
	"F8":        {Code: uv.KeyF8},
	"F9":        {Code: uv.KeyF9},
	"F10":       {Code: uv.KeyF10},
	"F11":       {Code: uv.KeyF11},
	"F12":       {Code: uv.KeyF12},
}

// keyNamed is the key name names: one of keys, or Ctrl- and a letter.
func keyNamed(name string) (uv.Key, bool) {
	if k, ok := keys[name]; ok {
		return k, true
	}
	if letter, ok := strings.CutPrefix(name, "Ctrl-"); ok && len(letter) == 1 {
		if c := letter[0] | 0x20; c >= 'a' && c <= 'z' {
			return uv.Key{Code: rune(c), Mod: uv.ModCtrl}, true
		}
	}
	return uv.Key{}, false
}

// escapeGap is how long Send waits after an Esc with input behind it: past
// the escape timeout of a program that reads Esc and the next key together
// as Alt and that key (tcell's, vim's ttimeoutlen).
const escapeGap = 150 * time.Millisecond

// enterGap is how long Send waits before an Enter that follows text: a
// program that tells a paste by its speed (Claude Code) would read an Enter
// in the same burst as a newline in a paste, not a key press.
const enterGap = 100 * time.Millisecond

// Send hands each step to the emulator, as a tab's typing goes, which
// encodes a key for the modes the program has set. Every step is checked
// before any is sent. The cursor it answers is where reader last stopped,
// so what anyone else at the terminal did since comes before the reply, or
// where the input went in for a reader that never read.
func (s *session) Send(reader string, input ...Input) (string, error) {
	events := make([]uv.KeyEvent, 0, len(input))
	for i, in := range input {
		switch {
		case (in.Text == "") == (in.Key == ""):
			return "", fmt.Errorf("%w, not both or neither: step %d", ErrBadInput, i+1)
		case in.Key != "":
			k, ok := keyNamed(in.Key)
			if !ok {
				return "", fmt.Errorf("%w %q: step %d", ErrUnknownKey, in.Key, i+1)
			}
			events = append(events, uv.KeyPressEvent(k))
		default:
			events = append(events, nil)
		}
	}
	s.revive()
	from := s.out.since(reader, s.out.offset())
	s.out.moved(reader, from)
	for i, ev := range events {
		if ev == nil {
			s.em.SendText(input[i].Text)
			continue
		}
		if input[i].Key == "Enter" && i > 0 && input[i-1].Text != "" {
			time.Sleep(enterGap)
		}
		s.em.SendKey(ev)
		if input[i].Key == "Esc" && i < len(events)-1 {
			time.Sleep(escapeGap)
		}
	}
	return s.out.cursor(from), nil
}

// Read reads the output log from cursor, or reader's position.
func (s *session) Read(reader, cursor string) (Output, error) {
	since, err := s.from(reader, cursor)
	if err != nil {
		return Output{}, err
	}
	return s.answered(reader, s.out.take(since), s.fullScreen()), nil
}

// Wait reads again each time the log changes, until one of its ends, and
// takes what it answers: what is held is kept, so the next read goes on
// from after it. While
// a full-screen program is up, until matches the screen's rows, and every
// change to the screen is one.
func (s *session) Wait(ctx context.Context, reader, cursor string, until *regexp.Regexp, idle time.Duration) (Output, bool, error) {
	since, err := s.from(reader, cursor)
	if err != nil {
		return Output{}, false, err
	}
	var timer *time.Timer
	var quiet <-chan time.Time
	if idle > 0 {
		timer = time.NewTimer(idle)
		defer timer.Stop()
		quiet = timer.C
	}
	for {
		changed := s.out.changed()
		got, screen := s.out.read(since), s.fullScreen()
		text := got.Text
		if screen != nil {
			text = strings.Join(screen.Rows, "\n")
		}
		if until != nil && until.MatchString(text) {
			s.redrawn(ctx)
			return s.answered(reader, s.out.take(since), s.fullScreen()), true, nil
		}
		select {
		case <-changed:
			if timer != nil {
				timer.Reset(idle)
			}
		case <-quiet:
			return s.answered(reader, s.out.take(since), screen), false, nil
		case <-ctx.Done():
			return s.answered(reader, s.out.take(since), screen), false, nil
		}
	}
}

// A match can land part way through a program redrawing a row, before the
// write that erases what is left of the old text: a wait that matches holds
// its answer until the screen has been unchanged for redrawQuiet, for at
// most redrawMost.
var (
	redrawQuiet = 50 * time.Millisecond
	redrawMost  = 250 * time.Millisecond
)

// redrawn returns once the screen has been unchanged for redrawQuiet, after
// redrawMost, or when ctx ends.
func (s *session) redrawn(ctx context.Context) {
	most := time.NewTimer(redrawMost)
	defer most.Stop()
	for {
		quiet := time.NewTimer(redrawQuiet)
		select {
		case <-s.out.changed():
			quiet.Stop()
		case <-quiet.C:
			return
		case <-most.C:
			quiet.Stop()
			return
		case <-ctx.Done():
			quiet.Stop()
			return
		}
	}
}

// from is where a read starts: cursor's offset, or reader's position when
// there is no cursor.
func (s *session) from(reader, cursor string) (int64, error) {
	if cursor == "" {
		return s.out.position(reader), nil
	}
	return s.out.at(cursor)
}

// answered moves reader's position to where got stopped, and answers got as
// an agent reads it, with screen when a full-screen program is up.
func (s *session) answered(reader string, got chunk, screen *Screen) Output {
	s.out.moved(reader, got.Next)
	return Output{Text: got.Text, Cursor: s.out.cursor(got.Next), Dropped: got.Dropped, Screen: screen}
}

// fullScreen is the screen while a full-screen program is up, nil when not.
func (s *session) fullScreen() *Screen {
	if screen := s.Screen(); screen.AltScreen {
		return &screen
	}
	return nil
}

// Screen reads the live screen under the screen lock, so output arriving
// meanwhile cannot tear it.
func (s *session) Screen() Screen {
	title, _ := s.titles()
	s.screen.RLock()
	defer s.screen.RUnlock()
	w, h := s.em.Width(), s.em.Height()
	rows := make([]string, h)
	for y := range h {
		var b strings.Builder
		for x := range w {
			switch c := s.em.CellAt(x, y); {
			case c == nil:
				b.WriteByte(' ')
			case c.Content == "" && c.Width == 0:
				// The tail of a wide character; the head carried it.
			default:
				b.WriteString(c.Content)
			}
		}
		rows[y] = strings.TrimRight(b.String(), " ")
	}
	at := s.em.CursorPosition()
	return Screen{Width: w, Height: h, Rows: rows, Cursor: Cursor{Row: at.Y, Col: at.X}, AltScreen: s.em.IsAltScreen(), Title: title}
}
