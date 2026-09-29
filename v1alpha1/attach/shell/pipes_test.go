package shell

import (
	"bytes"
	"errors"
	"io"
	"log/slog"
	"os"
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
			c := &cooked{echo: &echo, stdin: stdin, interrupt: func() { interrupted++ }}
			// A byte at a time, as keys arrive, and all at once, as a paste
			// does: the discipline has to read the same either way.
			for _, whole := range []bool{false, true} {
				echo.Reset()
				stdin.Reset()
				stdin.closed, interrupted = false, 0
				*c = cooked{echo: &echo, stdin: stdin, interrupt: func() { interrupted++ }}
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

// TestPipeArgs pins which programs are told they are interactive: a shell run
// bare, and nothing given arguments of its own or that is not a shell.
func TestPipeArgs(t *testing.T) {
	for _, tc := range []struct {
		path string
		args []string
		want []string
	}{
		{"/bin/sh", nil, []string{"-i"}},
		{"/usr/bin/bash", nil, []string{"-i"}},
		{`C:\Program Files\Git\bin\bash.exe`, nil, []string{"-i"}},
		{"/bin/sh", []string{"-c", "ls"}, []string{"-c", "ls"}},
		{"/usr/bin/htop", nil, nil},
		{"/usr/bin/shasum", nil, nil},
	} {
		if got := pipeArgs(tc.path, tc.args); !slices.Equal(got, tc.want) {
			t.Errorf("pipeArgs(%q, %q) = %q, want %q", tc.path, tc.args, got, tc.want)
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
	at := await("over-pipes\n", 0)

	began := time.Now()
	_, _ = io.WriteString(typed, "sleep 30\r")
	time.Sleep(300 * time.Millisecond)
	_, _ = io.WriteString(typed, "\x03")
	_, _ = io.WriteString(typed, "echo still-here\r")
	await("still-here\n", at)
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
}
