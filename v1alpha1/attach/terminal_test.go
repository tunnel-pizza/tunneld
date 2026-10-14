package attach

import (
	"context"
	"errors"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
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
			if _, err := term.Send(tc.input...); err != nil {
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
			_, err := term.Send(Input{Text: "echo hi"}, tc.bad)
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

	if _, err := Terminal(s.session).Send(Input{Text: "x"}); err != nil {
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

// TestTerminalReads pins the log through the handle: what the program
// printed, as text, and Send answering the offset its input was sent at, so
// what a reply prints is read from there.
func TestTerminalReads(t *testing.T) {
	target := newFakeTarget("api", true, true)
	target.out = "\x1b[1mbanner\x1b[0m\r\n"
	term := Terminal(serveFake(t, target).session)
	got := awaitRead(t, term, 0, "banner\n")
	if got.Next != 7 || got.Dropped {
		t.Errorf("Read(0) = %+v, want banner up to 7", got)
	}
	next, err := term.Send(Input{Text: "x"})
	if err != nil || next != 7 {
		t.Errorf("Send = %d, %v; want the offset the input went in at, 7", next, err)
	}
}

// TestTerminalWaits pins Wait's three ends: the pattern matching what came
// after since, the program falling quiet, and the context.
func TestTerminalWaits(t *testing.T) {
	target := newFakeTarget("api", true, true)
	target.out = "building\r\n"
	s := serveFake(t, target)
	term := Terminal(s.session)
	awaitRead(t, term, 0, "building\n")

	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = s.session.out.Write([]byte("step 1\r\n"))
		time.Sleep(50 * time.Millisecond)
		_, _ = s.session.out.Write([]byte("done: 0\r\n"))
	}()
	got, matched := term.Wait(t.Context(), 0, regexp.MustCompile(`done: \d`), time.Second)
	if !matched || got.Text != "building\nstep 1\ndone: 0\n" {
		t.Errorf("Wait for done = %+v, %v; want everything since 0, matched", got, matched)
	}

	start := time.Now()
	got, matched = term.Wait(t.Context(), got.Next, nil, 100*time.Millisecond)
	if matched || got.Text != "" || time.Since(start) < 100*time.Millisecond {
		t.Errorf("Wait for quiet = %+v, %v after %v; want nothing, unmatched, after the quiet", got, matched, time.Since(start))
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	got, matched = term.Wait(ctx, got.Next, regexp.MustCompile("never"), 0)
	if matched || ctx.Err() == nil {
		t.Errorf("Wait for never = %+v, %v; want unmatched once the context is done", got, matched)
	}
}

// TestTerminalLogResetsWithTheScreen pins a run starting again: the last
// run's output goes with its screen.
func TestTerminalLogResetsWithTheScreen(t *testing.T) {
	target := newRerunTarget(true)
	t.Cleanup(target.release)
	s := serveFake(t, target)
	target.awaitOver(t, 1)
	awaitEnded(t, s)
	if got := s.session.out.read(0); got.Text != "run 1" {
		t.Fatalf("read(0) = %+v, want the first run's output", got)
	}
	if _, err := Terminal(s.session).Send(Input{Text: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	target.awaitRun(t, 2)
	if got := awaitRead(t, Terminal(s.session), 0, "run 2"); !got.Dropped || got.Next != 10 {
		t.Errorf("after a restart, Read(0) = %+v, want run 2 alone up to 10, the first run dropped", got)
	}
}

// awaitRead is Read(since) once it reads want.
func awaitRead(t *testing.T, term Terminal, since int64, want string) Output {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got := term.Read(since)
		if got.Text == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("Read(%d) = %+v, want %q", since, got, want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestTerminalLeavesTheSizeAlone pins that an agent has no say in the
// terminal's size: it is no viewer, so typing, reading and waiting send
// the program nothing on its resize channel and leave the negotiated size
// as it was.
func TestTerminalLeavesTheSizeAlone(t *testing.T) {
	target := newFakeTarget("api", true, true)
	s := serveFake(t, target)
	select {
	case <-target.seenSz: // the run being told its size as it starts
	case <-time.After(5 * time.Second):
		t.Fatal("the run was never told its size")
	}
	before := s.session.size

	term := Terminal(s.session)
	if _, err := term.Send(Input{Text: "ls"}, Input{Key: "Enter"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_ = term.Screen()
	_ = term.Read(0)
	_, _ = term.Wait(t.Context(), 0, nil, 50*time.Millisecond)

	select {
	case size := <-target.seenSz:
		t.Errorf("the program was sent %+v, want no size from an agent", size)
	case <-time.After(300 * time.Millisecond):
	}
	s.session.mu.Lock()
	after := s.session.size
	s.session.mu.Unlock()
	if after != before {
		t.Errorf("size = %+v, want it left at %+v", after, before)
	}
}

// TestTerminalRestartKeepsThePersonsSize pins the one size an agent's call
// can lead to: typing into a program that exited runs it again, and the new
// run is told the size the person's window settled, not one of the
// agent's, as a tab arriving would.
func TestTerminalRestartKeepsThePersonsSize(t *testing.T) {
	target := newRerunTarget(true)
	target.holdFrom = 2
	t.Cleanup(target.release)
	s := serveFake(t, target)
	target.awaitRun(t, 1)
	target.awaitOver(t, 1)
	awaitEnded(t, s)

	settled := remotecommand.TerminalSize{Width: 100 - chromeWidth, Height: 40 - chromeHeight}
	c := dial(t, s)
	target.awaitRun(t, 2)
	readFrame(t, c)
	writeFrame(t, c, 4, `{"Width":100,"Height":40}`)
	target.awaitSize(t, settled)
	target.letOneGo()
	target.awaitOver(t, 2)
	awaitEnded(t, s)

	if _, err := Terminal(s.session).Send(Input{Text: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	target.awaitRun(t, 3)
	target.awaitSize(t, settled)
}
