package attach

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"sync"
	"time"
	"unicode/utf8"
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
	Text    string `json:"text" jsonschema:"the program's output, as text: control sequences and carriage returns dropped"`
	Cursor  string `json:"cursor" jsonschema:"where this read stopped: pass it back to read on from here"`
	Dropped bool   `json:"dropped" jsonschema:"whether output before text was no longer kept, so some was missed"`
}

// chunk is a read of the log by offset: the text, the offset after it, and
// whether some of what came after since was no longer kept.
type chunk struct {
	Text    string
	Next    int64
	Dropped bool
}

// output is a session's log: the text of everything its program printed,
// the control sequences a terminal acts on dropped, each byte at an offset
// that counts up for the life of the session.
type output struct {
	mu    sync.Mutex
	strip stripper
	size  int
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
	o := &output{
		size:    size,
		wake:    make(chan struct{}),
		readers: map[string]position{},
		now:     time.Now,
	}
	_, _ = rand.Read(o.id[:])
	return o
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

// Write keeps the text in p.
func (o *output) Write(p []byte) (int, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	before := len(o.buf)
	o.buf = o.strip.append(o.buf, p)
	if added := len(o.buf) - before; added > 0 {
		o.end += int64(added)
		if len(o.buf) > 2*o.size {
			o.buf = append(o.buf[:0], o.buf[len(o.buf)-o.size:]...)
		}
		o.notify()
	}
	return len(p), nil
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
	return chunk{Text: string(text), Next: o.end, Dropped: dropped}
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
	o.buf = o.buf[:0]
	o.strip = stripper{}
	o.notify()
}

// notify wakes whoever is waiting on changed. Called with mu held.
func (o *output) notify() {
	close(o.wake)
	o.wake = make(chan struct{})
}

// stripper drops what a terminal acts on from a byte stream and keeps the
// text, across writes: a sequence cut between two of them is still dropped
// whole. Like scanner, it branches only on 7-bit bytes, so UTF-8 passes
// through untouched.
type stripper struct{ state stripState }

type stripState int

const (
	stripGround    stripState = iota
	stripEsc                  // after ESC
	stripEscInter             // after ESC and an intermediate byte
	stripCSI                  // in ESC [ ... final
	stripString               // in an OSC, DCS, SOS, PM or APC
	stripStringEsc            // after ESC inside one, which ST (ESC \) ends
)

// append appends the text in p to dst.
func (s *stripper) append(dst, p []byte) []byte {
	for _, b := range p {
		switch s.state {
		case stripGround:
			switch {
			case b == 0x1b:
				s.state = stripEsc
			case b == '\n' || b == '\t' || (b >= 0x20 && b != 0x7f):
				dst = append(dst, b)
			}
		case stripEsc:
			s.escape(b)
		case stripEscInter:
			if b >= 0x30 && b <= 0x7e {
				s.state = stripGround
			}
		case stripCSI:
			if (b >= 0x40 && b <= 0x7e) || b == 0x18 || b == 0x1a {
				s.state = stripGround
			}
		case stripString:
			switch b {
			case 0x07, 0x18, 0x1a:
				s.state = stripGround
			case 0x1b:
				s.state = stripStringEsc
			}
		case stripStringEsc:
			if b == '\\' {
				s.state = stripGround
			} else {
				s.escape(b)
			}
		}
	}
	return dst
}

// escape is the byte after an ESC.
func (s *stripper) escape(b byte) {
	switch {
	case b == '[':
		s.state = stripCSI
	case b == ']' || b == 'P' || b == 'X' || b == '^' || b == '_':
		s.state = stripString
	case b == 0x1b:
		s.state = stripEsc
	case b >= 0x20 && b <= 0x2f:
		s.state = stripEscInter
	default:
		s.state = stripGround
	}
}
