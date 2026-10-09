package shell

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// stdinFake is a program's stdin: what it was sent, and whether it was
// closed.
type stdinFake struct {
	bytes.Buffer
	closed bool
}

func (s *stdinFake) Close() error { s.closed = true; return nil }

// TestCooked pins the line discipline tunneld stands in for over pipes: what
// a terminal's keys become on their way to a program reading lines, and what
// the person typing sees.
func TestCooked(t *testing.T) {
	for _, tc := range []struct {
		name        string
		keys        string
		sent        string
		echo        string
		interrupted int
		closed      bool
	}{
		{"Enter sends the line", "ls\r", "ls\n", "ls\r\n", 0, false},
		{"a pasted CRLF is one line", "ls\r\npwd\r\n", "ls\npwd\n", "ls\r\npwd\r\n", 0, false},
		{"a bare LF ends a line too", "ls\n", "ls\n", "ls\r\n", 0, false},
		{"Backspace takes a character off", "lx\x7fs\r", "ls\n", "lx\b \bs\r\n", 0, false},
		{"Backspace on an empty line is nothing", "\x7f\x7fls\r", "ls\n", "ls\r\n", 0, false},
		{"Backspace takes a whole rune", "é\x7fe\r", "e\n", "é\b \be\r\n", 0, false},
		{"Ctrl-U takes the line", "rm -rf\x15ls\r", "ls\n", "rm -rf" + strings.Repeat("\b \b", 6) + "ls\r\n", 0, false},
		{"Ctrl-C drops the line and interrupts", "sleep\x03", "", "sleep^C\r\n", 1, false},
		{"Ctrl-D on an empty line ends input", "\x04", "", "", 0, true},
		{"Ctrl-D mid-line sends it without a newline", "ab\x04", "ab", "ab", 0, false},
		{"arrow keys are dropped", "l\x1b[Ds\x1bOA\r", "ls\n", "ls\r\n", 0, false},
		{"Alt-key is dropped", "\x1bxls\r", "ls\n", "ls\r\n", 0, false},
		{"other control keys are dropped", "l\x01s\x02\r", "ls\n", "ls\r\n", 0, false},
		{"a tab is typed", "a\tb\r", "a\tb\n", "a\tb\r\n", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var echo bytes.Buffer
			stdin := &stdinFake{}
			interrupted := 0
			c := &cooked{echo: &echo, stdin: stdin, signal: func(k signalKey) {
				if k == sigInterrupt {
					interrupted++
				}
			}}
			// A byte at a time, as keys arrive, and all at once, as a paste
			// does: the discipline has to read the same either way.
			for _, whole := range []bool{false, true} {
				echo.Reset()
				stdin.Reset()
				stdin.closed, interrupted = false, 0
				*c = cooked{echo: &echo, stdin: stdin, signal: func(k signalKey) {
					if k == sigInterrupt {
						interrupted++
					}
				}}
				if whole {
					_, _ = c.Write([]byte(tc.keys))
				} else {
					for i := range len(tc.keys) {
						_, _ = c.Write([]byte{tc.keys[i]})
					}
				}
				if got := stdin.String(); got != tc.sent {
					t.Errorf("whole=%v: sent %q, want %q", whole, got, tc.sent)
				}
				if got := echo.String(); got != tc.echo {
					t.Errorf("whole=%v: echoed %q, want %q", whole, got, tc.echo)
				}
				if interrupted != tc.interrupted {
					t.Errorf("whole=%v: interrupted %d times, want %d", whole, interrupted, tc.interrupted)
				}
				if stdin.closed != tc.closed {
					t.Errorf("whole=%v: stdin closed = %v, want %v", whole, stdin.closed, tc.closed)
				}
			}
		})
	}
}

// TestCookedFollowsTheSettings pins the discipline when a program has set its
// terminal: raw mode passes bytes through as they come, escape sequences
// included; echo follows ECHO; the keys come from c_cc; ISIG off makes ^C a
// byte.
func TestCookedFollowsTheSettings(t *testing.T) {
	raw := defaultMode
	raw.lflag &^= lICANON | lECHO
	rawNoSig := raw
	rawNoSig.lflag &^= lISIG
	quiet := defaultMode
	quiet.lflag &^= lECHO
	hashErase := defaultMode
	hashErase.cc[vERASE] = '#'

	for _, tc := range []struct {
		name    string
		mode    settings
		keys    string
		sent    string
		echo    string
		signals []signalKey
	}{
		{"raw passes keys as they come", raw, "ihi\x1b:wq\r", "ihi\x1b:wq\n", "", nil},
		{"raw keeps arrows whole", raw, "\x1b[A", "\x1b[A", "", nil},
		{"raw still signals", raw, "a\x03b", "ab", "", []signalKey{sigInterrupt}},
		{"raw without ISIG sends ^C", rawNoSig, "\x03", "\x03", "", nil},
		{"raw without ICRNL keeps CR", func() settings { s := raw; s.iflag &^= iICRNL; return s }(), "\r", "\r", "", nil},
		{"ECHO off echoes nothing", quiet, "pw\r", "pw\n", "", nil},
		{"the erase key comes from c_cc", hashErase, "lx#s\r", "ls\n", "lx\b \bs\r\n", nil},
		{"^W erases a word", defaultMode, "git log\x17st\r", "git st\n", "git log\b \b\b \b\b \bst\r\n", nil},
		{"^Z suspends", defaultMode, "\x1a", "", "^Z\r\n", []signalKey{sigSuspend}},
		{"^\\ quits", defaultMode, "\x1c", "", "^\\\r\n", []signalKey{sigQuit}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var echo bytes.Buffer
			stdin := &stdinFake{}
			var got []signalKey
			c := &cooked{echo: &echo, stdin: stdin, mode: func() settings { return tc.mode },
				signal: func(k signalKey) { got = append(got, k) }}
			_, _ = c.Write([]byte(tc.keys))
			if stdin.String() != tc.sent {
				t.Errorf("sent %q, want %q", stdin.String(), tc.sent)
			}
			if echo.String() != tc.echo {
				t.Errorf("echoed %q, want %q", echo.String(), tc.echo)
			}
			if !slices.Equal(got, tc.signals) {
				t.Errorf("signals %v, want %v", got, tc.signals)
			}
		})
	}
}

// TestOproc pins output processing: CR before LF while OPOST and ONLCR are
// both set, and nothing at all once a program clears OPOST, as vi does.
func TestOproc(t *testing.T) {
	plain := defaultMode
	plain.oflag &^= oOPOST
	for _, tc := range []struct {
		name string
		mode func() settings
		in   string
		want string
	}{
		{"pipes add the CR", nil, "a\nb\n", "a\r\nb\r\n"},
		{"OPOST off adds nothing", func() settings { return plain }, "a\nb\n", "a\nb\n"},
	} {
		var got bytes.Buffer
		n, err := oproc{w: &got, mode: tc.mode}.Write([]byte(tc.in))
		if err != nil || n != len(tc.in) || got.String() != tc.want {
			t.Errorf("%s: wrote %q (%d, %v), want %q (%d)", tc.name, got.String(), n, err, tc.want, len(tc.in))
		}
	}
}

// TestPipeArgs pins which programs are told they are interactive — a shell
// run bare, and nothing given arguments of its own or that is not a shell —
// and which of those is bash, told to leave line editing to tunneld: by its
// name, or by the program a link named sh resolves to.
//
// Fixtures rather than the host's /bin/sh, which is dash on one machine and
// bash on the next.
func TestPipeArgs(t *testing.T) {
	dir := t.TempDir()
	sh := write(t, dir, "sh", 0o755)
	bashDir := t.TempDir()
	bash := write(t, bashDir, "bash", 0o755)
	linked := filepath.Join(t.TempDir(), "sh")
	symlinked := runtime.GOOS != "windows" && os.Symlink(bash, linked) == nil

	interactive := []string{"-i"}
	bashy := []string{"--noediting", "-i"}
	cases := []struct {
		path string
		args []string
		want []string
	}{
		{sh, nil, interactive},
		{bash, nil, bashy},
		{"/usr/bin/bash", nil, bashy},
		{`C:\Program Files\Git\bin\bash.exe`, nil, bashy},
		{"/bin/dash", nil, interactive},
		{sh, []string{"-c", "ls"}, []string{"-c", "ls"}},
		{bash, []string{"-c", "ls"}, []string{"-c", "ls"}},
		{"/usr/bin/htop", nil, nil},
		{"/usr/bin/shasum", nil, nil},
	}
	if symlinked {
		cases = append(cases, struct {
			path string
			args []string
			want []string
		}{linked, nil, bashy})
	}
	for _, tc := range cases {
		if got := pipeArgs(tc.path, tc.args); !slices.Equal(got, tc.want) {
			t.Errorf("pipeArgs(%q, %q) = %q, want %q", tc.path, tc.args, got, tc.want)
		}
	}
}

// TestOnlcr pins the newline a terminal would have added: every \n a program
// writes reaches the screen as \r\n, and the write reports the program's own
// length, not the longer one written.
func TestOnlcr(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"a\nb\n", "a\r\nb\r\n"},
		{"no newline", "no newline"},
		{"\n\n", "\r\n\r\n"},
		{"", ""},
	} {
		var got bytes.Buffer
		n, err := oproc{w: &got}.Write([]byte(tc.in))
		if err != nil || n != len(tc.in) {
			t.Errorf("Write(%q) = %d, %v; want %d, nil", tc.in, n, err, len(tc.in))
		}
		if got.String() != tc.want {
			t.Errorf("Write(%q) wrote %q, want %q", tc.in, got.String(), tc.want)
		}
	}
}

// noPTY stands for a machine with no pseudo-terminals to open.
func noPTY() (*os.File, *os.File, error) {
	return nil, nil, errors.New("no pseudo-terminal on this machine")
}

// TestOpenWithoutATerminal pins that a machine with no pseudo-terminals is
// not refused: the program is served over pipes, says so, and runs.
func TestOpenWithoutATerminal(t *testing.T) {
	dir := t.TempDir()
	path := write(t, dir, runnable("tunneld-fixture"), 0o755)

	var logged bytes.Buffer
	targets := &TargetsImpl{open: noPTY}
	target, err := targets.Open(t.Context(), path, nil, slog.New(slog.NewTextHandler(&logged, nil)))
	if err != nil {
		t.Fatalf("Open() = %v, want the program served over pipes", err)
	}
	defer func() { _ = target.Close() }()
	if target.TTY() || !target.Stdin() {
		t.Errorf("TTY() = %v, Stdin() = %v; want no terminal, and input", target.TTY(), target.Stdin())
	}
	if n := target.(*TargetImpl).Notice(); n != pipesNotice {
		t.Errorf("Notice() = %q, want %q", n, pipesNotice)
	}
	if !strings.Contains(logged.String(), "serving a program without a terminal") {
		t.Errorf("the log %q does not say the program has no terminal", logged.String())
	}

	out := &sink{}
	resize := make(chan remotecommand.TerminalSize)
	close(resize)
	if err := target.AttachContainer(t.Context(), "", "", "", strings.NewReader(""), out, out, false, resize); err != nil {
		t.Fatalf("AttachContainer() = %v", err)
	}
	if !strings.Contains(out.String(), "TUNNELD_FIXTURE_OK") {
		t.Errorf("output = %q, want the program's", out.String())
	}
}

// TestShellOverPipes pins a shell served over pipes the way a person uses one:
// it prompts, runs a line, and Ctrl-C ends what it is running and not the
// shell — the -i and the process group together.
func TestShellOverPipes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Ctrl-C over pipes is a process-group signal, which Windows has none of")
	}
	targets := &TargetsImpl{open: noPTY}
	target, err := targets.Open(t.Context(), "sh", nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer func() { _ = target.Close() }()

	in, typed := io.Pipe()
	out := &sink{}
	resize := make(chan remotecommand.TerminalSize)
	done := make(chan error, 1)
	go func() {
		done <- target.AttachContainer(t.Context(), "", "", "", in, out, out, false, resize)
	}()
	// A size is taken and dropped, rather than left for the sender to wait
	// on.
	select {
	case resize <- remotecommand.TerminalSize{Width: 80, Height: 24}:
	case <-time.After(5 * time.Second):
		t.Fatal("a size was never taken")
	}

	await := func(want string, after int) int {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if at := strings.Index(out.String()[after:], want); at >= 0 {
				return after + at + len(want)
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("waited for %q; the page shows %q", want, out.String())
		return 0
	}

	_, _ = io.WriteString(typed, "echo over-pipes\r")
	at := await("over-pipes\r\n", 0)

	began := time.Now()
	_, _ = io.WriteString(typed, "sleep 30\r")
	time.Sleep(300 * time.Millisecond)
	_, _ = io.WriteString(typed, "\x03")
	_, _ = io.WriteString(typed, "echo still-here\r")
	await("still-here\r\n", at)
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("Ctrl-C took %s to end sleep 30", took)
	}

	_, _ = io.WriteString(typed, "exit\r")
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("AttachContainer() = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the shell did not exit")
	}

	// No newline reached the page bare: a line feed alone moves down without
	// returning, and the screen staircases.
	shown := out.String()
	for i := range len(shown) {
		if shown[i] == '\n' && (i == 0 || shown[i-1] != '\r') {
			t.Fatalf("a bare newline at %d in %q", i, shown)
		}
	}
}

// TestBashOverPipes pins bash with its line editing off: tunneld echoes what
// is typed, so bash must not echo it again, and a Tab is a character in the
// line rather than a completion redrawn onto a terminal there is none of.
func TestBashOverPipes(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash over pipes is a Unix sandbox's; the Windows path is TestOpenWithoutATerminal's")
	}
	targets := &TargetsImpl{open: noPTY}
	target, err := targets.Open(t.Context(), "bash", nil, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Skipf("no bash here: %v", err)
	}
	defer func() { _ = target.Close() }()

	in, typed := io.Pipe()
	out := &sink{}
	resize := make(chan remotecommand.TerminalSize)
	close(resize)
	done := make(chan error, 1)
	go func() {
		done <- target.AttachContainer(t.Context(), "", "", "", in, out, out, false, resize)
	}()

	_, _ = io.WriteString(typed, "echo once-$((1+1))\r")
	_, _ = io.WriteString(typed, "echo tab\tafter\r")
	_, _ = io.WriteString(typed, "exit\r")
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("bash did not exit; the page shows %q", out.String())
	}

	shown := out.String()
	if n := strings.Count(shown, "echo once-$((1+1))"); n != 1 {
		t.Errorf("the line was echoed %d times in %q, want once", n, shown)
	}
	for _, want := range []string{"once-2\r\n", "tab after\r\n"} {
		if !strings.Contains(shown, want) {
			t.Errorf("the page shows %q, want %q in it", shown, want)
		}
	}
}
