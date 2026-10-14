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
	// the output's offset as the input went in: what the program prints in
	// reply is read from there. A step it cannot send sends nothing:
	// ErrUnknownKey for a key name it does not know, ErrBadInput for a step
	// that is not exactly one of text or key.
	Send(input ...Input) (int64, error)
	// Read is the program's output after since, as text.
	Read(since int64) Output
	// Wait is Read(since) once until matches its text, the program has
	// printed nothing for idle, or ctx is done, whichever is first, and
	// whether until matched. A nil until never matches, and an idle of zero
	// never ends it.
	Wait(ctx context.Context, since int64, until *regexp.Regexp, idle time.Duration) (Output, bool)
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

// Send hands each step to the emulator, as a tab's typing goes, which
// encodes a key for the modes the program has set. Every step is checked
// before any is sent.
func (s *session) Send(input ...Input) (int64, error) {
	events := make([]uv.KeyEvent, 0, len(input))
	for i, in := range input {
		switch {
		case (in.Text == "") == (in.Key == ""):
			return 0, fmt.Errorf("%w, not both or neither: step %d", ErrBadInput, i+1)
		case in.Key != "":
			k, ok := keyNamed(in.Key)
			if !ok {
				return 0, fmt.Errorf("%w %q: step %d", ErrUnknownKey, in.Key, i+1)
			}
			events = append(events, uv.KeyPressEvent(k))
		default:
			events = append(events, nil)
		}
	}
	s.revive()
	next := s.out.offset()
	for i, ev := range events {
		if ev == nil {
			s.em.SendText(input[i].Text)
			continue
		}
		s.em.SendKey(ev)
	}
	return next, nil
}

// Read is the output log's text after since.
func (s *session) Read(since int64) Output { return s.out.read(since) }

// Wait reads again each time the log changes, until one of its ends. What
// it answers on quiet or ctx is the last read, its Next where that read
// stopped, so nothing printed meanwhile is skipped by the next read.
func (s *session) Wait(ctx context.Context, since int64, until *regexp.Regexp, idle time.Duration) (Output, bool) {
	var timer *time.Timer
	var quiet <-chan time.Time
	if idle > 0 {
		timer = time.NewTimer(idle)
		defer timer.Stop()
		quiet = timer.C
	}
	for {
		changed := s.out.changed()
		got := s.out.read(since)
		if until != nil && until.MatchString(got.Text) {
			return got, true
		}
		select {
		case <-changed:
			if timer != nil {
				timer.Reset(idle)
			}
		case <-quiet:
			return got, false
		case <-ctx.Done():
			return got, false
		}
	}
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
	return Screen{Rows: rows, Cursor: Cursor{Row: at.Y, Col: at.X}, AltScreen: s.em.IsAltScreen(), Title: title}
}
