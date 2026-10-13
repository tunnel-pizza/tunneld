package attach

import (
	"errors"
	"log/slog"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// TestBoundTerminal pins which origins have a terminal: a served one does,
// by the index it has in the run, and a dialed one or an index past the run
// does not.
func TestBoundTerminal(t *testing.T) {
	display := shown(t, "http://localhost:3000", "exec:///usr/bin/htop")
	_, bound, err := New(WithTargets(&stubTargets{verb: v1.ExecScheme, local: true})).
		Bind(t.Context(), display, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	defer func() { _ = bound.Close() }()

	for index, want := range map[int]bool{0: false, 1: true, 2: false, -1: false} {
		if term, ok := bound.Terminal(index); ok != want || (term != nil) != want {
			t.Errorf("Terminal(%d) = %v, %v; want a terminal: %v", index, term, ok, want)
		}
	}
}

// TestTerminalSends pins input: each step in order, text as typed and keys
// by name, encoded by the emulator as a tab's keystrokes are.
func TestTerminalSends(t *testing.T) {
	for _, tc := range []struct {
		name  string
		input []Input
		want  string
	}{
		{"text", []Input{{Text: "echo hi"}}, "echo hi"},
		{"text, then Enter", []Input{{Text: "echo hi"}, {Key: "Enter"}}, "echo hi\r"},
		{"a key, then text", []Input{{Key: "Esc"}, {Text: ":wq"}, {Key: "Enter"}}, "\x1b:wq\r"},
		{"Ctrl-C", []Input{{Key: "Ctrl-C"}}, "\x03"},
		{"keys in order", []Input{{Key: "Tab"}, {Key: "Backspace"}, {Key: "Esc"}}, "\t\x7f\x1b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := newFakeTarget("api", true, true)
			term := Terminal(serveFake(t, target).session)
			if err := term.Send(tc.input...); err != nil {
				t.Fatalf("Send(%+v): %v", tc.input, err)
			}
			if got := readIn(t, target, tc.want); got != tc.want {
				t.Errorf("target read %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTerminalRefusesABadStep pins that a step Send cannot send, anywhere in
// the input, sends nothing, not even the steps before it.
func TestTerminalRefusesABadStep(t *testing.T) {
	for _, tc := range []struct {
		name string
		bad  Input
		is   error
		says string
	}{
		{"an unknown key", Input{Key: "Hyper-Q"}, ErrUnknownKey, "Hyper-Q"},
		{"text and a key", Input{Text: "ls", Key: "Enter"}, ErrBadInput, "step 2"},
		{"neither", Input{}, ErrBadInput, "step 2"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := newFakeTarget("api", true, true)
			term := Terminal(serveFake(t, target).session)
			err := term.Send(Input{Text: "echo hi"}, tc.bad)
			if !errors.Is(err, tc.is) || !strings.Contains(err.Error(), tc.says) {
				t.Fatalf("Send = %v, want %v saying %q", err, tc.is, tc.says)
			}
			select {
			case got := <-target.seenIn:
				t.Errorf("target read %q, want nothing", got)
			case <-time.After(200 * time.Millisecond):
			}
		})
	}
}

// TestTerminalScreen pins what an agent reads: the visible rows as text,
// trailing blanks dropped, the cursor, the title, and the alternate screen.
func TestTerminalScreen(t *testing.T) {
	target := newFakeTarget("api", true, true)
	target.out = "\x1b]2;my title\ahello\r\nworld"
	term := Terminal(serveFake(t, target).session)

	var got Screen
	deadline := time.Now().Add(5 * time.Second)
	for got = term.Screen(); got.Title != "my title" || len(got.Rows) < 2 || got.Rows[1] != "world"; got = term.Screen() {
		if time.Now().After(deadline) {
			t.Fatalf("Screen = %+v, want hello and world under my title", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
	if got.Rows[0] != "hello" || len(got.Rows) != defaultRows || slices.ContainsFunc(got.Rows[2:], func(r string) bool { return r != "" }) {
		t.Errorf("Rows = %q, want hello, world, then %d empty rows", got.Rows, defaultRows-2)
	}
	if want := (Cursor{Row: 1, Col: 5}); got.Cursor != want {
		t.Errorf("Cursor = %+v, want %+v", got.Cursor, want)
	}
	if got.AltScreen {
		t.Error("AltScreen = true, want the main screen")
	}
}

// TestTerminalStartsARepeatableTargetAgain pins that typing into a program
// that has exited runs it again, as a tab arriving does, through the same
// handle.
func TestTerminalStartsARepeatableTargetAgain(t *testing.T) {
	target := newRerunTarget(true)
	t.Cleanup(target.release)
	s := serveFake(t, target)
	target.awaitOver(t, 1)
	awaitEnded(t, s)

	if err := Terminal(s.session).Send(Input{Text: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	target.awaitRun(t, 2)
}

// readIn is what target reads on stdin until it has want, or what it had
// when the test's context ended.
func readIn(t *testing.T, target *fakeTarget, want string) string {
	t.Helper()
	var read strings.Builder
	for read.String() != want {
		select {
		case got := <-target.seenIn:
			read.WriteString(got)
			if !strings.HasPrefix(want, read.String()) {
				return read.String()
			}
		case <-time.After(5 * time.Second):
			return read.String()
		}
	}
	return read.String()
}
