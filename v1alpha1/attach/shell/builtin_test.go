package shell

import (
	"io"
	"log/slog"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// TestBuiltin pins the built-in shell's origin: this executable, as an
// ordinary exec:// program origin, with BuiltinArg as its only argument.
func TestBuiltin(t *testing.T) {
	origin, err := Builtin()
	if err != nil {
		t.Fatalf("Builtin() = %v", err)
	}
	u, err := url.Parse(origin)
	if err != nil {
		t.Fatalf("Builtin() = %q, not a URL: %v", origin, err)
	}
	if u.Scheme != v1.ExecScheme || u.Host != "" || u.Query()[v1.ArgKey][0] != BuiltinArg || len(u.Query()[v1.ArgKey]) != 1 {
		t.Errorf("Builtin() = %q, want exec:///<this executable>?%s=%s", origin, v1.ArgKey, BuiltinArg)
	}
	if _, ok := Resolve(u.Path); !ok {
		t.Errorf("Builtin()'s program %q does not resolve", u.Path)
	}
}

// TestBuiltinReexec pins the init hook: any binary linking this package,
// started with BuiltinArg alone, is the built-in shell and nothing else. The
// test binary links it, so it is the binary started; given no terminal, the
// shell reads its commands from stdin and exits with the status exit names.
func TestBuiltinReexec(t *testing.T) {
	cmd := exec.Command(os.Args[0], BuiltinArg)
	cmd.Stdin = strings.NewReader("echo reexec-ok\nexit 7\n")
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if !strings.Contains(string(out), "reexec-ok") {
		t.Errorf("output = %q, want the shell's", out)
	}
	if code := cmd.ProcessState.ExitCode(); code != 7 {
		t.Errorf("exit = %d (%v), want 7: the shell's own status", code, err)
	}
}

// TestBuiltinOnATerminal pins the built-in shell served the way tunneld serves
// it, on a pseudo-terminal, typed at a line at a time: an error is reported and
// the shell goes on, and Ctrl-C ends what is running and not the shell — the
// reason it is a process of its own rather than an interpreter inside tunneld.
func TestBuiltinOnATerminal(t *testing.T) {
	needsPTY(t)
	target, err := New().Open(t.Context(), os.Args[0], []string{BuiltinArg}, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Open() = %v", err)
	}
	defer func() { _ = target.Close() }()

	// Elvish's prompt ends in "> " after the working directory.
	const prompt = "> "
	in, typed := io.Pipe()
	out := &sink{}
	resize := make(chan remotecommand.TerminalSize)
	close(resize)
	done := make(chan error, 1)
	go func() {
		done <- target.AttachContainer(t.Context(), "", "", "", in, out, &sink{}, true, resize)
	}()

	await := func(want string, after int) int {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			if at := strings.Index(out.String()[after:], want); at >= 0 {
				return after + at + len(want)
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("waited for %q; the terminal shows %q", want, out.String())
		return 0
	}

	at := await(prompt, 0)

	_, _ = io.WriteString(typed, "no-such-command-anywhere\r")
	at = await("no-such-command-anywhere", at)
	_, _ = io.WriteString(typed, "echo after-the-error\r")
	at = await("after-the-error\r\n", at)

	began := time.Now()
	_, _ = io.WriteString(typed, "sleep 30\r")
	time.Sleep(500 * time.Millisecond)
	_, _ = io.WriteString(typed, "\x03")
	at = await(prompt, at)
	if took := time.Since(began); took > 10*time.Second {
		t.Errorf("Ctrl-C took %s to end sleep 30", took)
	}
	_, _ = io.WriteString(typed, "echo still-here\r")
	await("still-here\r\n", at)
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
