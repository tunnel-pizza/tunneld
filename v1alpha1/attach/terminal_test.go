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
			if _, err := term.Send("a", tc.input...); err != nil {
				t.Fatalf("Send(%+v): %v", tc.input, err)
			}
			if got := readIn(t, target, tc.want); got != tc.want {
				t.Errorf("target read %q, want %q", got, tc.want)
			}
		})
	}
}

// TestTerminalSeparatesAnEscape pins an Esc step with input after it:
// the program reads the Esc alone, escapeGap before the rest, so a program
// that reads Esc and a key together as Alt and that key (tcell, vim) sees
// the Esc it was sent.
func TestTerminalSeparatesAnEscape(t *testing.T) {
	target := newFakeTarget("api", true, true)
	term := Terminal(serveFake(t, target).session)
	sent := make(chan error, 1)
	go func() {
		_, err := term.Send("a", Input{Key: "Esc"}, Input{Text: "/local-path"}, Input{Key: "Enter"})
		sent <- err
	}()
	if got := readIn(t, target, "\x1b"); got != "\x1b" {
		t.Fatalf("first read %q, want the Esc alone", got)
	}
	start := time.Now()
	if got := readIn(t, target, "/local-path\r"); got != "/local-path\r" {
		t.Errorf("then read %q, want the rest", got)
	}
	if gap := time.Since(start); gap < escapeGap/2 {
		t.Errorf("the rest came %v after the Esc, want about %v", gap, escapeGap)
	}
	if err := <-sent; err != nil {
		t.Errorf("Send: %v", err)
	}
}

// TestTerminalSeparatesAnEnterFromText pins an Enter step right after text
// in one call: the program reads the text, then the Enter enterGap later,
// so one that tells a paste by its speed reads a key press, not a newline
// in a paste.
func TestTerminalSeparatesAnEnterFromText(t *testing.T) {
	target := newFakeTarget("api", true, true)
	term := Terminal(serveFake(t, target).session)
	sent := make(chan error, 1)
	go func() {
		_, err := term.Send("a", Input{Text: "a long prompt"}, Input{Key: "Enter"})
		sent <- err
	}()
	if got := readIn(t, target, "a long prompt"); got != "a long prompt" {
		t.Fatalf("first read %q, want the text alone", got)
	}
	start := time.Now()
	if got := readIn(t, target, "\r"); got != "\r" {
		t.Errorf("then read %q, want the Enter", got)
	}
	if gap := time.Since(start); gap < enterGap/2 {
		t.Errorf("the Enter came %v after the text, want about %v", gap, enterGap)
	}
	if err := <-sent; err != nil {
		t.Errorf("Send: %v", err)
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
			_, err := term.Send("a", Input{Text: "echo hi"}, tc.bad)
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
	if got.Width != defaultCols || got.Height != defaultRows {
		t.Errorf("size = %dx%d, want %dx%d", got.Width, got.Height, defaultCols, defaultRows)
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

	if _, err := Terminal(s.session).Send("a", Input{Text: "x"}); err != nil {
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
// printed, as text; each reader from where it last stopped, the start for
// one never seen, and no reader's read moving another's; a cursor reading
// from where it says whoever passes it; and Send answering the cursor its
// input went in at, so what a reply prints is read from there.
func TestTerminalReads(t *testing.T) {
	target := newFakeTarget("api", true, true)
	target.out = "\x1b[1mbanner\x1b[0m\r\n"
	s := serveFake(t, target)
	term := Terminal(s.session)
	awaitRead(t, term, s.session.out.cursor(0), "banner\n")

	read := func(reader, cursor string) Output {
		t.Helper()
		got, err := term.Read(reader, cursor)
		if err != nil {
			t.Fatalf("Read(%q, %q): %v", reader, cursor, err)
		}
		return got
	}
	if got := read("a", ""); got.Text != "banner\n" || got.Dropped || got.Cursor != s.session.out.cursor(7) {
		t.Errorf("a's first read = %+v, want banner, up to 7", got)
	}
	if got := read("a", ""); got.Text != "" {
		t.Errorf("a's second read = %+v, want nothing new", got)
	}
	if got := read("b", ""); got.Text != "banner\n" {
		t.Errorf("b's first read = %+v, want banner: a's reads are not b's", got)
	}
	sent, err := term.Send("b", Input{Text: "x"})
	if err != nil || sent != s.session.out.cursor(7) {
		t.Errorf("Send = %q, %v; want the cursor the input went in at, 7", sent, err)
	}
	if got := read("a", s.session.out.cursor(0)); got.Text != "banner\n" {
		t.Errorf("a's read from the start's cursor = %+v, want banner again", got)
	}
}

// TestTerminalAnswersAFullScreen pins reads and waits while a full-screen
// program is up: each answer carries the screen, until matches its rows,
// and a wait goes quiet once the screen stops changing. On the main screen
// an answer carries none.
func TestTerminalAnswersAFullScreen(t *testing.T) {
	target := newFakeTarget("api", true, true)
	s := serveFake(t, target)
	term := Terminal(s.session)
	out := &sink{s.session}
	_, _ = out.Write([]byte("$ top\r\n"))
	if got, err := term.Read("a", ""); err != nil || got.Screen != nil {
		t.Errorf("Read on the main screen = %+v, %v; want no screen", got, err)
	}

	_, _ = out.Write([]byte("\x1b[?1049h\x1b[2J\x1b[HCPU 3%"))
	got, err := term.Read("a", "")
	if err != nil || got.Screen == nil || got.Screen.Rows[0] != "CPU 3%" || !got.Screen.AltScreen {
		t.Fatalf("Read on a full screen = %+v, %v; want the screen, CPU 3%% on top", got, err)
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = out.Write([]byte("\x1b[HCPU 42%"))
	}()
	got, matched, err := term.Wait(t.Context(), "a", "", regexp.MustCompile(`CPU 4\d%`), 0)
	if err != nil || !matched || got.Screen == nil || got.Screen.Rows[0] != "CPU 42%" {
		t.Errorf("Wait for CPU 4x%% = %+v, %v, %v; want it matched on the screen", got, matched, err)
	}

	start := time.Now()
	go func() {
		for range 3 {
			time.Sleep(40 * time.Millisecond)
			_, _ = out.Write([]byte("\x1b[HCPU 7% "))
		}
	}()
	got, matched, err = term.Wait(t.Context(), "a", "", nil, 100*time.Millisecond)
	if err != nil || matched || got.Screen == nil || time.Since(start) < 200*time.Millisecond {
		t.Errorf("Wait for quiet = %+v, %v, %v after %v; want the screen once it stopped changing", got, matched, err, time.Since(start))
	}
}

// TestTerminalRefusesACursorNotItsOwn pins a cursor from another terminal,
// or from before tunneld started again, as ErrStaleCursor, and one no
// terminal gave as ErrBadCursor, for Read and Wait alike.
func TestTerminalRefusesACursorNotItsOwn(t *testing.T) {
	term := Terminal(serveFake(t, newFakeTarget("api", true, true)).session)
	other := newOutput(1 << 10).cursor(0)
	if _, err := term.Read("a", other); !errors.Is(err, ErrStaleCursor) {
		t.Errorf("Read with another log's cursor: %v, want ErrStaleCursor", err)
	}
	if _, _, err := term.Wait(t.Context(), "a", other, nil, time.Millisecond); !errors.Is(err, ErrStaleCursor) {
		t.Errorf("Wait with another log's cursor: %v, want ErrStaleCursor", err)
	}
	if _, err := term.Read("a", "junk"); !errors.Is(err, ErrBadCursor) {
		t.Errorf("Read with junk: %v, want ErrBadCursor", err)
	}
}

// TestTerminalWaits pins Wait's three ends: the pattern matching what came
// after the cursor, the program falling quiet, and the context; and a wait
// with no cursor reading on from where the reader's last one stopped.
func TestTerminalWaits(t *testing.T) {
	target := newFakeTarget("api", true, true)
	target.out = "building\r\n"
	s := serveFake(t, target)
	term := Terminal(s.session)
	start := s.session.out.cursor(0)
	awaitRead(t, term, start, "building\n")

	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = s.session.out.Write([]byte("step 1\r\n"))
		time.Sleep(50 * time.Millisecond)
		_, _ = s.session.out.Write([]byte("done: 0\r\n"))
	}()
	got, matched, err := term.Wait(t.Context(), "a", start, regexp.MustCompile(`done: \d`), time.Second)
	if err != nil || !matched || got.Text != "building\nstep 1\ndone: 0\n" {
		t.Errorf("Wait for done = %+v, %v, %v; want everything since the start, matched", got, matched, err)
	}

	start2 := time.Now()
	got, matched, err = term.Wait(t.Context(), "a", "", nil, 100*time.Millisecond)
	if err != nil || matched || got.Text != "" || time.Since(start2) < 100*time.Millisecond {
		t.Errorf("Wait for quiet from a's position = %+v, %v, %v after %v; want nothing, unmatched, after the quiet", got, matched, err, time.Since(start2))
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	got, matched, err = term.Wait(ctx, "a", got.Cursor, regexp.MustCompile("never"), 0)
	if err != nil || matched || ctx.Err() == nil {
		t.Errorf("Wait for never = %+v, %v, %v; want unmatched once the context is done", got, matched, err)
	}
}

// TestTerminalLogResetsWithTheScreen pins a run starting again: the last
// run's output goes with its screen, and a cursor from before reads what
// is left, saying some was dropped.
func TestTerminalLogResetsWithTheScreen(t *testing.T) {
	target := newRerunTarget(true)
	t.Cleanup(target.release)
	s := serveFake(t, target)
	target.awaitOver(t, 1)
	awaitEnded(t, s)
	if got := s.session.out.read(0); got.Text != "run 1\n" {
		t.Fatalf("read(0) = %+v, want the first run's output", got)
	}
	if _, err := Terminal(s.session).Send("a", Input{Text: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	target.awaitRun(t, 2)
	if got := awaitRead(t, Terminal(s.session), s.session.out.cursor(0), "run 2\n"); !got.Dropped || got.Cursor != s.session.out.cursor(12) {
		t.Errorf("after a restart, a read from the start = %+v, want run 2 alone up to 12, the first run dropped", got)
	}
}

// awaitRead is a read from cursor once it reads want.
func awaitRead(t *testing.T, term Terminal, cursor, want string) Output {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := term.Read("await", cursor)
		if err == nil && got.Text == want {
			return got
		}
		if time.Now().After(deadline) {
			t.Fatalf("Read(%q) = %+v, %v; want %q", cursor, got, err, want)
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
	if _, err := term.Send("a", Input{Text: "ls"}, Input{Key: "Enter"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	_ = term.Screen()
	_, _ = term.Read("a", "")
	_, _, _ = term.Wait(t.Context(), "a", "", nil, 50*time.Millisecond)

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

// TestTerminalSendAnswersFromTheReadersPosition pins Send's cursor on a
// terminal someone else uses too: from where this reader last stopped, so
// what the other did since comes before the reply, and for a reader that
// never read, where its input went in.
func TestTerminalSendAnswersFromTheReadersPosition(t *testing.T) {
	target := newFakeTarget("api", true, true)
	s := serveFake(t, target)
	term := Terminal(s.session)
	out := &sink{s.session}
	_, _ = out.Write([]byte("$ first\r\n"))
	if _, err := term.Read("a", ""); err != nil {
		t.Fatal(err)
	}
	_, _ = out.Write([]byte("$ theirs\r\nanswered\r\n"))
	sent, err := term.Send("a", Input{Text: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got, _ := term.Read("a", sent); got.Text != "$ theirs\nanswered\n" {
		t.Errorf("a read from Send's cursor = %q, want the other's exchange first", got.Text)
	}
	if sent, _ := term.Send("new", Input{Text: "y"}); sent != s.session.out.cursor(s.session.out.offset()) {
		t.Errorf("a new reader's Send = %q, want where its input went in", sent)
	}
}

// TestTerminalWaitLetsARedrawFinish pins a match that lands while a
// program is part way through redrawing a row: the answer is the row once
// the redraw has finished, not torn.
func TestTerminalWaitLetsARedrawFinish(t *testing.T) {
	target := newFakeTarget("api", true, true)
	s := serveFake(t, target)
	out := &sink{s.session}
	go func() {
		time.Sleep(30 * time.Millisecond)
		_, _ = out.Write([]byte("✻ done 7:29 PMh high effort)"))
		time.Sleep(20 * time.Millisecond)
		_, _ = out.Write([]byte("\x1b[14D\x1b[K"))
	}()
	got, matched, err := Terminal(s.session).Wait(t.Context(), "a", s.session.out.cursor(0), regexp.MustCompile(`done \d+:\d+`), 0)
	if err != nil || !matched || got.Text != "✻ done 7:29 PM" {
		t.Errorf("Wait = %q, %v, %v; want the row once its redraw finished", got.Text, matched, err)
	}
}

// TestTerminalReadsNoTitle pins a title the program sets: it names the
// terminal, and is not text a read answers, whatever characters it has.
func TestTerminalReadsNoTitle(t *testing.T) {
	target := newFakeTarget("api", true, true)
	s := serveFake(t, target)
	_, _ = (&sink{s.session}).Write([]byte("❯ \x1b]0;✳ Pong reply\x07"))
	got, err := Terminal(s.session).Read("a", s.session.out.cursor(0))
	if err != nil || got.Text != "❯ " {
		t.Errorf("Read = %q, %v; want the prompt alone", got.Text, err)
	}
}

// TestTerminalReadsAtThePersonsSize pins the log following the size the
// person's window settled: a cursor sent past the right edge stops at
// theirs.
func TestTerminalReadsAtThePersonsSize(t *testing.T) {
	target := newFakeTarget("api", true, true)
	s := serveFake(t, target)
	c := dial(t, s)
	readFrame(t, c)
	writeFrame(t, c, 4, `{"Width":50,"Height":20}`)
	width := 50 - chromeWidth
	logWidth := func() int {
		s.session.out.mu.Lock()
		defer s.session.out.mu.Unlock()
		return s.session.out.em.Width()
	}
	for logWidth() != width {
		select {
		case <-target.seenSz:
		case <-time.After(5 * time.Second):
			t.Fatalf("the log is %d wide, want %d", logWidth(), width)
		}
	}
	_, _ = (&sink{s.session}).Write([]byte("x\x1b[200Cy\r\n"))
	got, err := Terminal(s.session).Read("a", s.session.out.cursor(0))
	if want := "x" + strings.Repeat(" ", width-2) + "y\n"; err != nil || got.Text != want {
		t.Errorf("Read = %q, %v; want %q", got.Text, err, want)
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

	if _, err := Terminal(s.session).Send("a", Input{Text: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	target.awaitRun(t, 3)
	target.awaitSize(t, settled)
}
