package attach

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/vt"
)

// outputSize is how much of a program's output a session's log keeps, at
// least.
const outputSize = 1 << 20

// readerIdle is how long a reader's position is kept unused.
const readerIdle = time.Hour

// ErrBadCursor is a cursor no terminal gave.
var ErrBadCursor = errors.New("attach: not a cursor")

// ErrStaleCursor is a cursor from another terminal, or from this one before
// tunneld started again: its offsets mean nothing in this log.
var ErrStaleCursor = errors.New("attach: a cursor from another terminal, or from before tunneld started again")

// Output is a stretch of a program's output as text, the cursor to read
// from next, and whether some of what came before it was no longer kept.
type Output struct {
	Text    string  `json:"text" jsonschema:"the program's output as the terminal shows it, a line wider than the screen kept whole: the line the cursor is on last, as far as the cursor"`
	Cursor  string  `json:"cursor" jsonschema:"where this read stopped: pass it back to read on from here"`
	Dropped bool    `json:"dropped" jsonschema:"whether output before text was no longer kept, so some was missed"`
	Screen  *Screen `json:"screen,omitempty" jsonschema:"the screen, as terminal_screen gives it, while a full-screen program is up: it leaves no text"`
}

// chunk is a read of the log by offset: the text, the offset after it, and
// whether some of what came after since was no longer kept.
type chunk struct {
	Text    string
	Next    int64
	Dropped bool
}

// output is a session's log: every line its program printed as the
// terminal shows it, each byte at an offset that counts up for the life of
// the session. A line is in the log once the program is done with it, once
// it scrolls off the screen or the cursor moves below it; the line the
// cursor is on is read as far as the cursor, and is not yet at an offset.
type output struct {
	mu   sync.Mutex
	size int
	// em is the log's own screen, the same size as the one the person sees,
	// so a line wraps where theirs does. Its scrollback is emptied after
	// every write: what it held is what left the screen since the last.
	em *vt.Emulator
	// logged is the text of the screen's first rows, those already in buf,
	// as they were when they went in.
	logged []string
	// held is the rows above the cursor not yet in buf, as the log keeps
	// them: a row a program is redrawing (a spinner, a status block) goes in
	// once, as it stands when it scrolls off, a reader takes it, input goes
	// in, or the screen is erased.
	held string
	// line is the cursor's line, as far as the cursor, when it is not in buf.
	line string
	// alt is whether a full-screen program's screen was up after the last
	// write: every write to it is a change, though it leaves no text.
	alt bool
	// placed is a sequence about to set the cursor, from placedFrom: the
	// move it makes is not a wrap.
	placed     bool
	placedFrom uv.Position
	// reached is the lowest row the cursor has been on since the last
	// settle; erased the rows an ED 2 copied into the scrollback since.
	reached, erased int
	// buf is the kept text, oldest first, ending at offset end. It holds at
	// least size bytes once that much has arrived, and at most twice that:
	// compacting to size only once it doubles keeps a busy program's every
	// write from copying the whole log.
	buf  []byte
	end  int64
	wake chan struct{}

	// id is this log's, random: a cursor carries it, so one from another
	// log, another terminal's or this one's before tunneld started again,
	// is refused rather than read as an offset into the wrong text.
	id [8]byte
	// readers is each reader's position, by reader, and when it was last
	// moved; one unused for readerIdle is forgotten.
	readers map[string]position
	now     func() time.Time
}

type position struct {
	at   int64
	used time.Time
}

func newOutput(size int) *output {
	em := vt.NewEmulator(defaultCols, defaultRows)
	// Room for every line one write can scroll off, emptied after each.
	em.SetScrollbackSize(size)
	// What the emulator answers a program goes nowhere: the session's own
	// screen answers it. Unread, it would block the next write.
	go func() { _, _ = io.Copy(io.Discard, em) }()
	o := &output{
		em:      em,
		size:    size,
		wake:    make(chan struct{}),
		readers: map[string]position{},
		now:     time.Now,
	}
	em.SetCallbacks(vt.Callbacks{CursorPosition: o.moveCursor})
	// Every sequence that can put the cursor back to the left: CUU, CUD,
	// CUF, CUB, CNL, CPL, CHA, CUP, CHT, CBT, HPA, HPR, VPA, VPR, HVP, and
	// DECRC. Each passes on to the emulator's own handler.
	for _, cmd := range []int{'A', 'B', 'C', 'D', 'E', 'F', 'G', 'H', 'I', 'Z', '`', 'a', 'd', 'e', 'f'} {
		em.RegisterCsiHandler(cmd, func(ansi.Params) bool { return o.place() })
	}
	em.RegisterEscHandler('8', o.place)
	// A whole-screen erase (ED 2, ED 3) ends what was on the screen: the
	// rows with text above the lowest the cursor reached since the last
	// write are kept first (a clear homes the cursor before it erases), and
	// stay logged, so a program repainting the same rows logs them once. A
	// full-screen program's erase is its own screen's. x/vt's ED 3 blanks the screen as well as the
	// scrollback.
	em.RegisterCsiHandler('J', func(p ansi.Params) bool {
		if n, _, _ := p.Param(0, 0); (n == 2 || n == 3) && !o.em.IsAltScreen() {
			reached := max(o.reached, o.em.CursorPosition().Y)
			o.settle()
			bottom := 0
			for y := range reached {
				if t, _ := text(o.em.Width(), o.cells(y)); t != "" {
					bottom = y + 1
				}
			}
			o.keepRows(bottom)
			// ED 2 copies every row with text into the scrollback as it
			// clears: those did not scroll off, and are not lines to log.
			if n == 2 {
				for y := range o.em.Height() {
					if t, _ := text(o.em.Width(), o.cells(y)); t != "" {
						o.erased++
					}
				}
			}
		}
		return false
	})
	_, _ = rand.Read(o.id[:])
	return o
}

// wrapMark is on the last cell of a row the terminal wrapped onto the next.
// The log's screen is never drawn, so the mark is a link parameter no
// program sets: it moves with the row as the screen scrolls, into the
// scrollback, and goes when the cell is written again.
const wrapMark = "tunneld-wrap"

// place notes a sequence about to set the cursor. It handles nothing.
func (o *output) place() bool {
	o.placed, o.placedFrom = true, o.em.CursorPosition()
	return false
}

// moveCursor marks the row above the cursor wrapped when the cursor went
// from the last column straight to the column after the first character
// of the next row: an autowrap, which no control character does on a
// screen wider than four columns, and a sequence that does is placed.
func (o *output) moveCursor(from, to uv.Position) {
	o.reached = max(o.reached, from.Y, to.Y)
	if o.placed {
		o.placed = false
		if from == o.placedFrom {
			return
		}
	}
	width := o.em.Width()
	if width <= 4 || to.Y == 0 || to.Y != from.Y || from.X != width-1 || to.X < 1 || to.X > 2 || o.em.IsAltScreen() {
		return
	}
	x := width - 1
	c := o.em.CellAt(x, to.Y-1)
	if c != nil && c.Width == 0 {
		x--
		c = o.em.CellAt(x, to.Y-1)
	}
	if c == nil {
		return
	}
	marked := *c
	marked.Link.Params = wrapMark
	o.em.SetCell(x, to.Y-1, &marked)
}

// cursor is offset at in this log, opaque: the log's id, then the offset.
func (o *output) cursor(at int64) string {
	b := make([]byte, 16)
	copy(b, o.id[:])
	binary.BigEndian.PutUint64(b[8:], uint64(at))
	return base64.RawURLEncoding.EncodeToString(b)
}

// at is the offset cursor names in this log.
func (o *output) at(cursor string) (int64, error) {
	b, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil || len(b) != 16 {
		return 0, ErrBadCursor
	}
	if [8]byte(b[:8]) != o.id {
		return 0, ErrStaleCursor
	}
	return int64(binary.BigEndian.Uint64(b[8:])), nil
}

// position is where reader last stopped, the start for a reader with none.
func (o *output) position(reader string) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.forget()
	return o.readers[reader].at
}

// since is where reader last stopped, or fallback for a reader with none.
func (o *output) since(reader string, fallback int64) int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.forget()
	if p, ok := o.readers[reader]; ok {
		return min(p.at, fallback)
	}
	return fallback
}

// moved puts reader's position at at.
func (o *output) moved(reader string, at int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.forget()
	o.readers[reader] = position{at: at, used: o.now()}
}

// forget drops the readers unused for readerIdle. Called with mu held.
func (o *output) forget() {
	for reader, p := range o.readers {
		if o.now().Sub(p.used) > readerIdle {
			delete(o.readers, reader)
		}
	}
}

// Write draws p on the log's screen, and keeps the lines it finished.
func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, _ = o.em.Write(p)
	o.settle()
	return len(p), nil
}

// resize follows the screen the person sees.
func (o *output) resize(width, height int) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.em.Resize(width, height)
	o.settle()
}

// settle moves into buf each line that left the screen without being
// logged, and holds the rows above the cursor. A line that left is logged
// unless it is the next logged row, unchanged (past blank ones, which a
// clear leaves in place); a logged row the screen no longer shows, and
// every row below it, is held again. A full-screen
// program's screen is its own and leaves nothing, but every write to it is
// a change. Called with mu held.
func (o *output) settle() {
	before, shown := len(o.buf), o.held+o.line
	sb := o.em.Scrollback()
	left := sb.Lines()
	left = left[min(o.erased, len(left)):]
	o.erased = 0
	same, j := 0, 0
	for same < len(left) && j < len(o.logged) {
		switch l := left[same]; {
		case logged(len(l), l.At) == o.logged[j]:
			same, j = same+1, j+1
		case o.logged[j] == "\n":
			j++ // a clear moves only the rows with text
		default:
			j = len(o.logged)
		}
	}
	for _, l := range left[same:] {
		o.buf = append(o.buf, logged(len(l), l.At)...)
	}
	o.logged = o.logged[min(len(left), len(o.logged)):]
	sb.Clear()
	o.held, o.line = "", ""
	if !o.em.IsAltScreen() {
		for y, was := range o.logged {
			if o.row(y) != was {
				o.logged = o.logged[:y]
				break
			}
		}
		at := o.em.CursorPosition()
		var held strings.Builder
		for y := len(o.logged); y < at.Y; y++ {
			held.WriteString(o.row(y))
		}
		o.held = held.String()
		if len(o.logged) <= at.Y {
			// Faint text from the cursor on is a placeholder or ghost
			// text, not input: a suggestion in an empty input box.
			row := o.cells(at.Y)
			o.line, _ = text(o.em.Width(), func(x int) *uv.Cell {
				if c := row(x); c == nil || x < at.X || c.Style.Attrs&uv.AttrFaint == 0 {
					return c
				}
				return nil
			})
			if pad := at.X - ansi.StringWidth(o.line); pad > 0 {
				o.line += strings.Repeat(" ", pad)
			}
		}
	}
	o.reached = o.em.CursorPosition().Y
	alt := o.alt
	o.alt = o.em.IsAltScreen()
	if added := o.grew(before); added > 0 || o.held+o.line != shown || alt || o.alt {
		o.notify()
	}
}

// keepHeld moves the rows above the cursor into buf, as the screen has
// them now. Called with mu held.
func (o *output) keepHeld() { o.keepRows(o.em.CursorPosition().Y) }

// keepRows moves the rows from the last logged one up to row bottom into
// buf. Called with mu held.
func (o *output) keepRows(bottom int) {
	before := len(o.buf)
	for y := len(o.logged); y < bottom; y++ {
		row := o.row(y)
		o.buf = append(o.buf, row...)
		o.logged = append(o.logged, row)
	}
	o.held = ""
	o.grew(before)
}

// grew counts what buf took on since it was before bytes long, and
// compacts it. Called with mu held.
func (o *output) grew(before int) int {
	added := len(o.buf) - before
	o.end += int64(added)
	if len(o.buf) > 2*o.size {
		o.buf = append(o.buf[:0], o.buf[len(o.buf)-o.size:]...)
	}
	return added
}

// row is row y of the log's screen as the log keeps it.
func (o *output) row(y int) string { return logged(o.em.Width(), o.cells(y)) }

// cells is row y of the log's screen, a cell at a time.
func (o *output) cells(y int) func(x int) *uv.Cell {
	return func(x int) *uv.Cell { return o.em.CellAt(x, y) }
}

// logged is a row as the log keeps it: its text and a newline, or its text
// alone when the terminal wrapped it onto the next row.
func logged(width int, at func(x int) *uv.Cell) string {
	t, wraps := text(width, at)
	if wraps {
		return t
	}
	return t + "\n"
}

// text is a row of width cells as text, and whether it wraps onto the next
// row: a row that does keeps the blanks it ends with, as part of its line,
// and one that does not drops them.
func text(width int, at func(x int) *uv.Cell) (string, bool) {
	var b strings.Builder
	wraps := false
	for x := range width {
		c := at(x)
		switch {
		case c == nil:
			b.WriteByte(' ')
		case c.Content == "" && c.Width == 0:
			// The tail of a wide character; the head carried it.
		default:
			b.WriteString(c.Content)
		}
		if c != nil && x >= width-2 && c.Link.Params == wrapMark {
			wraps = true
		}
	}
	if wraps {
		return b.String(), true
	}
	return strings.TrimRight(b.String(), " "), false
}

// read is the text after since, from the oldest kept when since is older,
// and from the end when it is past it.
func (o *output) read(since int64) chunk {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.readLocked(since)
}

// take is read, with what it reads kept first: a row a reader has seen is
// in the log, and a later read goes on from after it.
func (o *output) take(since int64) chunk {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.keepHeld()
	return o.readLocked(since)
}

// readLocked is read, called with mu held.
func (o *output) readLocked(since int64) chunk {
	start := o.end - int64(len(o.buf))
	since = min(since, o.end)
	dropped := since < start
	if dropped {
		since = start
	}
	text := o.buf[since-start:]
	if dropped {
		// The compaction may have cut a character; start at the next whole one.
		for len(text) > 0 && !utf8.RuneStart(text[0]) {
			text = text[1:]
		}
	}
	return chunk{Text: string(text) + o.held + o.line, Next: o.end, Dropped: dropped}
}

// offset is where the next text will start, the held rows kept first.
func (o *output) offset() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.keepHeld()
	return o.end
}

// changed closes the next time text arrives, or the log is reset.
func (o *output) changed() <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.wake
}

// reset drops what the last run printed, and any sequence it left half
// written. Offsets keep counting, past what was held.
func (o *output) reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.keepHeld()
	_, _ = o.em.Write([]byte("\x1bc"))
	o.em.ClearScrollback()
	o.buf = o.buf[:0]
	o.logged, o.held, o.line = nil, "", ""
	o.notify()
}

// notify wakes whoever is waiting on changed. Called with mu held.
func (o *output) notify() {
	close(o.wake)
	o.wake = make(chan struct{})
}
