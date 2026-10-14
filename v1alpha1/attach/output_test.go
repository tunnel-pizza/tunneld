package attach

import (
	"strings"
	"testing"
)

// TestOutputStripsTheTerminal pins what the log keeps of a program's
// output: the text, with newlines and tabs, and none of the control
// sequences, carriage returns or other controls a terminal acts on, however
// the stream is cut.
func TestOutputStripsTheTerminal(t *testing.T) {
	for _, tc := range []struct {
		name   string
		stream string
		want   string
	}{
		{"plain text", "hello\r\nworld\n", "hello\nworld\n"},
		{"colour", "\x1b[1;31mred\x1b[0m plain", "red plain"},
		{"a title", "\x1b]0;my title\x07after", "after"},
		{"a title ended by ST", "\x1b]2;t\x1b\\after", "after"},
		{"bracketed paste mode", "\x1b[?2004h$ \x1b[?2004l", "$ "},
		{"a charset", "\x1b(Bok", "ok"},
		{"a two-byte escape", "\x1b=\x1b>ok", "ok"},
		{"a DCS", "\x1bP1$r0m\x1b\\ok", "ok"},
		{"tabs and controls", "a\tb\x07\x08c", "a\tbc"},
		{"UTF-8", "✳ 末 “quoted”", "✳ 末 “quoted”"},
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

// TestOutputOffsets pins reading from an offset: the text after it and the
// next offset to read from, with dropped saying a reader fell behind what
// is kept.
func TestOutputOffsets(t *testing.T) {
	l := newOutput(8)
	_, _ = l.Write([]byte("abcd"))
	if got := l.read(0); got.Text != "abcd" || got.Next != 4 || got.Dropped {
		t.Errorf("read(0) = %+v, want abcd up to 4", got)
	}
	if got := l.read(2); got.Text != "cd" || got.Next != 4 {
		t.Errorf("read(2) = %+v, want cd up to 4", got)
	}
	if got := l.read(4); got.Text != "" || got.Next != 4 {
		t.Errorf("read(4) = %+v, want nothing new", got)
	}
	if got := l.read(99); got.Text != "" || got.Next != 4 {
		t.Errorf("read(99) = %+v, want nothing, from the end", got)
	}

	_, _ = l.Write([]byte(strings.Repeat("x", 20)))
	got := l.read(0)
	if !got.Dropped || got.Next != 24 || len(got.Text) < 8 || strings.Trim(got.Text, "x") != "" {
		t.Errorf("read(0) after 24 bytes in an 8-byte log = %+v, want dropped, at least the last 8 bytes, up to 24", got)
	}
	if got := l.read(20); got.Dropped || got.Text != "xxxx" {
		t.Errorf("read(20) = %+v, want the last 4, nothing dropped", got)
	}
}

// TestOutputResets pins a run starting again: what the last run printed is
// gone, offsets keep counting so an agent's offset still means something,
// and a reader is told it was woken.
func TestOutputResets(t *testing.T) {
	l := newOutput(1 << 10)
	_, _ = l.Write([]byte("run 1\x1b]0;half a ti"))
	woken := l.changed()
	l.reset()
	select {
	case <-woken:
	default:
		t.Error("reset woke nobody")
	}
	_, _ = l.Write([]byte("run 2"))
	if got := l.read(0); got.Text != "run 2" || got.Next != 10 || !got.Dropped {
		t.Errorf("read(0) after a reset = %+v, want run 2 alone up to 10, the first run dropped", got)
	}
}

// TestOutputWakes pins changed: it closes when text arrives, and not for
// output that leaves no text.
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
