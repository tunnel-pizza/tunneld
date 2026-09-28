//go:build unix

package pid_test

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/tunnel-pizza/tunneld/v1alpha1/pid"
)

// TestDetach pins what a detached run does once its addresses are out: the
// run's stdout and stderr stop being the caller's — so a caller reading them
// to the end gets there while the run goes on — the launcher gets SIGUSR2, and
// what the run says afterwards lands in the file it was given.
//
// Detach moves this process's own descriptors, so it runs in a child: this
// test binary again, told by the environment to be the run, with this
// process as the launcher.
func TestDetach(t *testing.T) {
	if dir := os.Getenv("TUNNELD_TEST_DETACH"); dir != "" {
		// The log file, opened the way the log opens it: appended to, so the
		// detached streams and the logger's own writes share it.
		out, err := os.OpenFile(filepath.Join(dir, "run.log"), os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			os.Exit(3)
		}
		p := pid.New(pid.WithDir(dir), pid.WithParent(os.Getppid()))
		fmt.Println("https://t.example/")
		if !p.Detach(out, slog.New(slog.NewTextHandler(os.Stderr, nil))) {
			os.Exit(2)
		}
		fmt.Println("after, on stdout")
		fmt.Fprintln(os.Stderr, "after, on stderr")
		time.Sleep(3 * time.Second)
		os.Exit(0)
	}

	signals := make(chan os.Signal, 1)
	signal.Notify(signals, syscall.SIGUSR2)
	defer signal.Stop(signals)

	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], "-test.run=^TestDetach$")
	cmd.Env = append(os.Environ(), "TUNNELD_TEST_DETACH="+dir)
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cmd.Process.Kill(); cmd.Wait() })

	drain := func(r io.Reader) chan string {
		ch := make(chan string, 1)
		go func() {
			b, _ := io.ReadAll(r)
			ch <- string(b)
		}()
		return ch
	}
	outCh, errCh := drain(stdout), drain(stderr)

	select {
	case <-signals:
	case <-time.After(10 * time.Second):
		t.Fatal("no SIGUSR2 from the run")
	}
	var out, errOut string
	select {
	case out = <-outCh:
		errOut = <-errCh
	case <-time.After(2 * time.Second):
		t.Fatal("the caller's streams are still held after the signal")
	}

	if out != "https://t.example/\n" {
		t.Errorf("caller's stdout = %q, want the address and nothing after it", out)
	}
	logs, _ := filepath.Glob(filepath.Join(dir, "*.log"))
	if len(logs) != 1 {
		t.Fatalf("logs = %q, want one", logs)
	}
	if errOut != "" {
		t.Errorf("caller's stderr = %q, want nothing: the launcher says where the log is", errOut)
	}
	if cmd.Process.Signal(syscall.Signal(0)) != nil {
		t.Error("the run ended before its streams were let go, which proves nothing")
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		body, _ := os.ReadFile(logs[0])
		if strings.Contains(string(body), "after, on stdout\n") && strings.Contains(string(body), "after, on stderr\n") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s = %q, want what the run said afterwards", logs[0], body)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
