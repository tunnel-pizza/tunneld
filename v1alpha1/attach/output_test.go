package attach

import (
	"errors"
	"fmt"
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

// TestOutputJoinsSoftWraps pins a line wider than the screen: logged
// whole, where the terminal wrapped it, however the stream is cut. A line
// that fills the screen exactly and then breaks stays a line, and a column
// set from the last by a sequence is no wrap.
func TestOutputJoinsSoftWraps(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
		want   string
	}{
		{"output wider than the screen", "abcdefghijklmno\r\n", "abcdefghijklmno\n"},
		{"three rows", "abcdefghij0123456789ABCDE\r\n", "abcdefghij0123456789ABCDE\n"},
		{"a space at the wrap", "abcdefghi jklm\r\n", "abcdefghi jklm\n"},
		{"the line in progress", "abcdefghijklm", "abcdefghijklm"},
		{"exactly the width", "0123456789\r\nnext\r\n", "0123456789\nnext\n"},
		{"a shell crossing the edge as zsh does", "$ echo aft \r\x1b[Ke\rer\r\r\nafter\r\n$ ", "$ echo after\nafter\n$ "},
		{"scrolled off, wrapped", strings.Repeat("abcdefghijklmno\r\n", 6), strings.Repeat("abcdefghijklmno\n", 6)},
		{"a column set from the last", "first\r\n0123456789\x1b[2Gx\r\n", "first\n0x23456789\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for cut := range len(tc.stream) + 1 {
				l := newOutput(1 << 10)
				l.resize(10, 4)
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
// stays, and every line drawn after is logged, each once. The log keeps
// what is on the screen between writes, as a reader taking it does.
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
				_ = l.take(0)
			}
			if got := l.read(0).Text; got != tc.want {
				t.Errorf("read(0) = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOutputHoldsRowsThatChange pins a program redrawing rows in place
// (a spinner, a status block): while they change they are read, but not
// logged, so the log has their last state once, not every frame.
func TestOutputHoldsRowsThatChange(t *testing.T) {
	l := newOutput(1 << 16)
	_, _ = l.Write([]byte("> ask\r\n"))
	_ = l.take(0)
	_, _ = l.Write([]byte("thinking 0\r\n───\r\n> "))
	for i := range 50 {
		_, _ = l.Write([]byte(fmt.Sprintf("\x1b[2A\r\x1b[Jthinking %d\r\n───\r\n> ", i)))
		if got := l.read(0).Text; !strings.Contains(got, fmt.Sprintf("thinking %d\n", i)) {
			t.Fatalf("frame %d reads %q, want it", i, got)
		}
	}
	_, _ = l.Write([]byte("\x1b[2A\r\x1b[J⏺ pong\r\n───\r\n> "))
	got := l.take(0)
	if got.Text != "> ask\n⏺ pong\n───\n> " || strings.Contains(got.Text, "thinking") {
		t.Errorf("take(0) = %q, want the last frame alone", got.Text)
	}
	if again := l.read(got.Next); again.Text != "> " {
		t.Errorf("read after a take = %q, want only the line in progress", again.Text)
	}
}

// TestOutputKeepsRowsAClearErases pins a clear arriving while rows are
// held: they are kept first, as the screen showed them, so a command's
// output followed straight away by a clear is not lost.
func TestOutputKeepsRowsAClearErases(t *testing.T) {
	for _, clear := range []string{"\x1b[H\x1b[2J", "\x1b[3J\x1b[H\x1b[2J"} {
		l := newOutput(1 << 10)
		_, _ = l.Write([]byte("a\r\n\r\nb\r\n"))
		_, _ = l.Write([]byte(clear + "c\r\n"))
		if got := l.take(0).Text; got != "a\n\nb\nc\n" {
			t.Errorf("after %q, take(0) = %q, want a, a blank, b, then c", clear, got)
		}
	}
}

// TestOutputLeavesOutAPlaceholder pins faint text from the cursor on, on
// the line in progress: a suggestion drawn in an empty input box, or ghost
// text after what was typed, is not input, and is left out. Faint text the
// cursor is past, and text that is not faint, are kept.
func TestOutputLeavesOutAPlaceholder(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
		want   string
	}{
		{"a suggestion in an empty box", "❯\u00a0\x1b[2mTry \"how does x work?\"\x1b[22m\x1b[3G", "❯\u00a0"},
		{"ghost text after what was typed", "❯ hello\x1b[2m world\x1b[22m\x1b[8G", "❯ hello"},
		{"faint text the cursor is past", "\x1b[2mdim\x1b[22m ok", "dim ok"},
		{"plain text after the cursor", "❯ hello world\x1b[8G", "❯ hello world"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			l := newOutput(1 << 10)
			_, _ = l.Write([]byte(tc.stream))
			if got := l.read(0).Text; got != tc.want {
				t.Errorf("read(0) = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestOutputKeepsARepaintOnce pins a program repainting the whole screen
// (clear, then the same rows again, as Claude Code does): what the log
// has is not logged again, and what is new after it is.
func TestOutputKeepsARepaintOnce(t *testing.T) {
	l := newOutput(1 << 10)
	_, _ = l.Write([]byte("banner\r\n> ask\r\nreply\r\n> "))
	_ = l.take(0)
	_, _ = l.Write([]byte("\x1b[H\x1b[2Jbanner\r\n> ask\r\nreply\r\n> "))
	if got := l.take(0).Text; got != "banner\n> ask\nreply\n> " {
		t.Errorf("after a repaint, take(0) = %q, want each row once", got)
	}
	_, _ = l.Write([]byte("\x1b[H\x1b[2Jbanner\r\n> ask\r\nreply\r\nnew\r\n> "))
	if got := l.take(0).Text; got != "banner\n> ask\nreply\nnew\n> " {
		t.Errorf("after a repaint with a new row, take(0) = %q, want the new row once", got)
	}
}

// TestOutputHoldsUnreadRows pins rows nobody reads: however long the
// screen is still, they stay held until a read takes them, so a spinner
// that changes slowly logs no frame of its own.
func TestOutputHoldsUnreadRows(t *testing.T) {
	l := newOutput(1 << 10)
	_, _ = l.Write([]byte("a\r\nthinking 0\r\n> "))
	for i := 1; i <= 3; i++ {
		time.Sleep(400 * time.Millisecond)
		_, _ = l.Write([]byte(fmt.Sprintf("\x1b[1A\r\x1b[2Kthinking %d\r\n> ", i)))
	}
	if got := l.read(0); got.Next != 0 {
		t.Fatalf("read(0) = %+v, want everything still held", got)
	}
	if got := l.take(0).Text; got != "a\nthinking 3\n> " {
		t.Errorf("take(0) = %q, want the last frame alone", got)
	}
}

// TestOutputKeepsRowsRewrittenAsTheyScroll pins rows the log has that a
// program writes over and scrolls off in one write: what they became is
// logged, not taken for what they were.
func TestOutputKeepsRowsRewrittenAsTheyScroll(t *testing.T) {
	l := newOutput(1 << 10)
	l.resize(10, 4)
	_, _ = l.Write([]byte("1\r\n2\r\n3\r\n"))
	_ = l.take(0)
	_, _ = l.Write([]byte("\x1b[3A\rX\r\nY\r\nZ\r\n4\r\n5\r\n6\r\n7\r\n"))
	if got := l.take(0).Text; got != "1\n2\n3\nX\nY\nZ\n4\n5\n6\n7\n" {
		t.Errorf("take(0) = %q, want X, Y and Z kept as they scrolled off", got)
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
	if got := l.take(0); got.Text != "a\nb\n$ " || got.Next != 4 || got.Dropped {
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
	got := l.take(0)
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
	_ = l.take(0)
	woken := l.changed()
	l.reset()
	select {
	case <-woken:
	default:
		t.Error("reset woke nobody")
	}
	_, _ = l.Write([]byte("run 2\r\n"))
	if got := l.take(0); got.Text != "run 2\n" || got.Next != 12 || !got.Dropped {
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
