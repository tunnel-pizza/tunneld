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
	// line is the cursor's line, as far as the cursor, when it is not in buf.
	line string
	// alt is whether a full-screen program's screen was up after the last
	// write: every write to it is a change, though it leaves no text.
	alt bool
	// placed is a sequence about to set the cursor, from placedFrom: the
	// move it makes is not a wrap.
	placed     bool
	placedFrom uv.Position
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

// settle moves into buf each line the program is done with, once: those
// that left the screen without being logged, then those above the cursor.
// What left the screen went from logged first; a logged row the screen no
// longer shows, and every row below it, is logged again once the cursor is
// past it. A full-screen program's screen is its own and leaves nothing,
// but every write to it is a change. Called with mu held.
func (o *output) settle() {
	before, line := len(o.buf), o.line
	sb := o.em.Scrollback()
	left := sb.Lines()
	gone := min(len(left), len(o.logged))
	for _, l := range left[gone:] {
		o.buf = append(o.buf, logged(len(l), l.At)...)
	}
	o.logged = o.logged[gone:]
	sb.Clear()
	o.line = ""
	if !o.em.IsAltScreen() {
		for y, was := range o.logged {
			if o.row(y) != was {
				o.logged = o.logged[:y]
				break
			}
		}
		at := o.em.CursorPosition()
		for y := len(o.logged); y < at.Y; y++ {
			row := o.row(y)
			o.buf = append(o.buf, row...)
			o.logged = append(o.logged, row)
		}
		if len(o.logged) == at.Y {
			o.line, _ = text(o.em.Width(), o.cells(at.Y))
			if pad := at.X - ansi.StringWidth(o.line); pad > 0 {
				o.line += strings.Repeat(" ", pad)
			}
		}
	}
	alt := o.alt
	o.alt = o.em.IsAltScreen()
	if added := len(o.buf) - before; added > 0 || o.line != line || alt || o.alt {
		o.end += int64(added)
		if len(o.buf) > 2*o.size {
			o.buf = append(o.buf[:0], o.buf[len(o.buf)-o.size:]...)
		}
		o.notify()
	}
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
	return chunk{Text: string(text) + o.line, Next: o.end, Dropped: dropped}
}

// offset is where the next text will start.
func (o *output) offset() int64 {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.end
}

// changed closes the next time text arrives, or the log is reset.
func (o *output) changed() <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.wake
}

// reset drops what the last run printed, and any sequence it left half
// written. Offsets keep counting.
func (o *output) reset() {
	o.mu.Lock()
	defer o.mu.Unlock()
	_, _ = o.em.Write([]byte("\x1bc"))
	o.em.ClearScrollback()
	o.buf = o.buf[:0]
	o.logged, o.line = nil, ""
	o.notify()
}

// notify wakes whoever is waiting on changed. Called with mu held.
func (o *output) notify() {
	close(o.wake)
	o.wake = make(chan struct{})
}
