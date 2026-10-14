package attach

import (
	"errors"
	"strings"
	"testing"
	"time"
)

// TestOutputRendersTheTerminal pins what the log keeps of a program's
// output: each line as the terminal shows it, what a redraw overwrote gone
// and no control sequence left, the cursor's line last as far as the
// cursor, however the stream is cut.
func TestOutputRendersTheTerminal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
		want   string
	}{
		{"lines", "hello\r\nworld\r\n", "hello\nworld\n"},
		{"colour", "\x1b[1;31mred\x1b[0m plain", "red plain"},
		{"a title", "\x1b]0;my title\x07after", "after"},
		{"a title ended by ST", "\x1b]2;t\x1b\\after", "after"},
		{"bracketed paste mode", "\x1b[?2004h$ \x1b[?2004l", "$ "},
		{"a charset", "\x1b(Bok", "ok"},
		{"a two-byte escape", "\x1b=\x1b>ok", "ok"},
		{"a DCS", "\x1bP1$r0m\x1b\\ok", "ok"},
		{"a tab", "a\tb", "a       b"},
		{"UTF-8", "✳ 末 “quoted”", "✳ 末 “quoted”"},
		{"a shell redrawing what was typed", "% l\x08ls\r\r\nCLAUDE.md\r\n% ", "% ls\nCLAUDE.md\n% "},
		{"a carriage return", "50%\r100%\r\n", "100%\n"},
		{"a line cleared", "abc\r\x1b[Kxy", "xy"},
		{"zsh's partial-line mark", "%" + strings.Repeat(" ", 79) + "\r \r$ ", "$ "},
		{"a full-screen program", "$ htop\r\n\x1b[?1049h\x1b[2J\x1b[HCPU 3%\x1b[?1049l$ ", "$ htop\n$ "},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Every way of cutting the stream in two reads the same.
			for cut := range len(tc.stream) + 1 {
				l := newOutput(1 << 10)
				_, _ = l.Write([]byte(tc.stream[:cut]))
				_, _ = l.Write([]byte(tc.stream[cut:]))
				if got := l.read(0).Text; got != tc.want {
					t.Fatalf("cut at %d: %q, want %q", cut, got, tc.want)
				}
			}
		})
	}
}

// TestOutputRedraws pins lines the log already has that the screen then
// changes: a clear, and a row rewritten above the cursor. What was logged
// stays, and every line drawn after is logged, each once.
func TestOutputRedraws(t *testing.T) {
	for _, tc := range []struct {
		name   string
		writes []string
		want   string
	}{
		{"a clear", []string{"a\r\n\r\nb\r\n", "\x1b[H\x1b[2Jc\r\nd\r\n"}, "a\n\nb\nc\nd\n"},
		{"a clear, then a line at a time", []string{"a\r\n\r\nb\r\n", "\x1b[H\x1b[2J", "c\r\n", "d\r\n"}, "a\n\nb\nc\nd\n"},
		{"a row rewritten above the cursor", []string{"one\r\ntwo\r\n", "\x1b[Aowt\r\n"}, "one\ntwo\nowt\n"},
		{"a scroll after a clear", []string{"a\r\n\r\nb\r\n", "\x1b[H\x1b[2J", strings.Repeat("c\r\n", 30)}, "a\n\nb\n" + strings.Repeat("c\n", 30)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newOutput(1 << 10)
			for _, w := range tc.writes {
				_, _ = l.Write([]byte(w))
			}
			if got := l.read(0).Text; got != tc.want {
				t.Errorf("read(0) = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOutputScrolls pins lines that scroll off the screen: each kept once,
// in order, however many go in one write.
func TestOutputScrolls(t *testing.T) {
	l := newOutput(1 << 16)
	var want strings.Builder
	for i := range 100 {
		_, _ = l.Write([]byte(strings.Repeat("x", i%7) + "\r\n"))
		want.WriteString(strings.Repeat("x", i%7) + "\n")
	}
	var burst strings.Builder
	for i := range 60 {
		burst.WriteString(strings.Repeat("y", i%5) + "\r\n")
		want.WriteString(strings.Repeat("y", i%5) + "\n")
	}
	_, _ = l.Write([]byte(burst.String()))
	if got := l.read(0).Text; got != want.String() {
		t.Errorf("read(0) = %q, want %q", got, want.String())
	}
}

// TestOutputFollowsTheSize pins resize: a line wraps where the terminal
// the person sees wraps it.
func TestOutputFollowsTheSize(t *testing.T) {
	l := newOutput(1 << 10)
	l.resize(4, 3)
	_, _ = l.Write([]byte("abcdef\r\n"))
	if got := l.read(0).Text; got != "abcd\nef\n" {
		t.Errorf("read(0) = %q, want abcd and ef, wrapped at 4", got)
	}
}

// TestOutputOffsets pins reading from an offset: the text after it and the
// next offset to read from, with dropped saying a reader fell behind what
// is kept. The line the cursor is on is read each time, until the cursor
// leaves it.
func TestOutputOffsets(t *testing.T) {
	l := newOutput(8)
	_, _ = l.Write([]byte("a\r\nb\r\n$ "))
	if got := l.read(0); got.Text != "a\nb\n$ " || got.Next != 4 || got.Dropped {
		t.Errorf("read(0) = %+v, want a, b and the prompt, up to 4", got)
	}
	if got := l.read(2); got.Text != "b\n$ " || got.Next != 4 {
		t.Errorf("read(2) = %+v, want b and the prompt, up to 4", got)
	}
	if got := l.read(4); got.Text != "$ " || got.Next != 4 {
		t.Errorf("read(4) = %+v, want the prompt again", got)
	}
	if got := l.read(99); got.Text != "$ " || got.Next != 4 {
		t.Errorf("read(99) = %+v, want the prompt, from the end", got)
	}

	_, _ = l.Write([]byte("ls\r\n" + strings.Repeat("x\r\n", 9)))
	got := l.read(0)
	if !got.Dropped || got.Next != 27 || len(got.Text) < 8 || strings.Trim(got.Text, "x\n") != "" {
		t.Errorf("read(0) after 27 bytes in an 8-byte log = %+v, want dropped, at least the last 8 bytes, up to 27", got)
	}
	if got := l.read(23); got.Dropped || got.Text != "x\nx\n" {
		t.Errorf("read(23) = %+v, want the last two lines, nothing dropped", got)
	}
}

// TestOutputResets pins a run starting again: what the last run printed is
// gone, offsets keep counting so an agent's offset still means something,
// and a reader is told it was woken.
func TestOutputResets(t *testing.T) {
	l := newOutput(1 << 10)
	_, _ = l.Write([]byte("run 1\r\n\x1b]0;half a ti"))
	woken := l.changed()
	l.reset()
	select {
	case <-woken:
	default:
		t.Error("reset woke nobody")
	}
	_, _ = l.Write([]byte("run 2\r\n"))
	if got := l.read(0); got.Text != "run 2\n" || got.Next != 12 || !got.Dropped {
		t.Errorf("read(0) after a reset = %+v, want run 2 alone up to 12, the first run dropped", got)
	}
}

// TestOutputWakes pins changed: it closes when what a read answers changes,
// the line the cursor is on included, and not for output that changes
// nothing.
func TestOutputWakes(t *testing.T) {
	l := newOutput(1 << 10)
	ch := l.changed()
	_, _ = l.Write([]byte("\x1b[0m"))
	select {
	case <-ch:
		t.Fatal("changed closed for output with no text")
	default:
	}
	_, _ = l.Write([]byte("hi"))
	select {
	case <-ch:
	default:
		t.Fatal("changed did not close for text")
	}
}

// TestOutputWakesForAFullScreen pins changed on a full-screen program's
// screen, which leaves no text: every write to it closes changed, so a wait
// can tell the screen moved.
func TestOutputWakesForAFullScreen(t *testing.T) {
	l := newOutput(1 << 10)
	_, _ = l.Write([]byte("\x1b[?1049h"))
	ch := l.changed()
	_, _ = l.Write([]byte("\x1b[HCPU 3%"))
	select {
	case <-ch:
	default:
		t.Fatal("changed did not close for a full-screen program's drawing")
	}
	if got := l.read(0).Text; got != "" {
		t.Errorf("read(0) = %q, want no text from a full-screen program", got)
	}
}

// TestOutputCursors pins a cursor: an offset this log gave reads back as
// itself, one from another log (another terminal, or this one before
// tunneld started again) is ErrStaleCursor, and anything else ErrBadCursor.
func TestOutputCursors(t *testing.T) {
	l := newOutput(1 << 10)
	for _, at := range []int64{0, 5, 1 << 40} {
		got, err := l.at(l.cursor(at))
		if err != nil || got != at {
			t.Errorf("at(cursor(%d)) = %d, %v; want %d", at, got, err, at)
		}
	}
	if _, err := l.at(newOutput(1 << 10).cursor(5)); !errors.Is(err, ErrStaleCursor) {
		t.Errorf("another log's cursor: %v, want ErrStaleCursor", err)
	}
	for _, bad := range []string{"", "x", "not a cursor at all", l.cursor(5) + "AA"} {
		if _, err := l.at(bad); !errors.Is(err, ErrBadCursor) {
			t.Errorf("at(%q) = %v, want ErrBadCursor", bad, err)
		}
	}
}

// TestOutputReaders pins a reader's position: none until it reads, from
// the start; where it last stopped after; one reader's never another's;
// and forgotten once unused for readerIdle.
func TestOutputReaders(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	l := newOutput(1 << 10)
	l.now = func() time.Time { return now }
	if got := l.position("a"); got != 0 {
		t.Errorf("a new reader's position = %d, want the start", got)
	}
	l.moved("a", 7)
	if got := l.position("a"); got != 7 {
		t.Errorf("position after moving to 7 = %d", got)
	}
	if got := l.position("b"); got != 0 {
		t.Errorf("b's position = %d, want its own, the start", got)
	}
	now = now.Add(readerIdle - time.Second)
	if got := l.position("a"); got != 7 {
		t.Errorf("a's position just inside the idle time = %d, want 7", got)
	}
	now = now.Add(readerIdle + time.Second)
	if got := l.position("a"); got != 0 {
		t.Errorf("a's position after an idle hour = %d, want forgotten", got)
	}
}
