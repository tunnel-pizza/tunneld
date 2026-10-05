package shell

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"os/exec"
	"runtime"
	"strings"
	"testing"
	"time"
)

// spawnable is a resolved target over a real program, as Open would make
// one, with no terminal involved.
func spawnable(t *testing.T, name string, args ...string) *TargetImpl {
	t.Helper()
	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("no %s on this machine: %v", name, err)
	}
	return &TargetImpl{ref: name, path: path, args: args, log: slog.New(slog.DiscardHandler)}
}

// TestSpawn pins what an agent's one-shot command gets: stdout and stderr
// apart, the real exit code, stdin reaching the program, and a result that
// does not wait on what the program left behind.
func TestSpawn(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the rows run sh")
	}
	for _, tc := range []struct {
		name       string
		argv       []string
		stdin      string
		wantOut    string
		wantErr    string
		wantExit   int
		wantWithin time.Duration
	}{
		{"stdout and stderr are apart", []string{"-c", "echo out; echo err >&2"}, "", "out\n", "err\n", 0, 0},
		{"the exit code is the program's", []string{"-c", "exit 3"}, "", "", "", 3, 0},
		{"stdin reaches the program", []string{"-c", "cat"}, "fed\n", "fed\n", "", 0, 0},
		{"nil argv runs the origin's own", nil, "echo via-stdin\n", "via-stdin\n", "", 0, 0},
		{"empty argv runs the origin's own", []string{}, "echo via-stdin\n", "via-stdin\n", "", 0, 0},
		{"a background child does not hold the result", []string{"-c", "sleep 5 & echo hi"}, "", "hi\n", "", 0, pipeWait + 3*time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			target := spawnable(t, "sh")
			var out, errb bytes.Buffer
			var stdin io.Reader
			if tc.stdin != "" {
				stdin = strings.NewReader(tc.stdin)
			}
			start := time.Now()
			exit, err := target.Spawn(t.Context(), tc.argv, stdin, &out, &errb)
			if err != nil {
				t.Fatalf("Spawn: %v", err)
			}
			if tc.wantWithin > 0 && time.Since(start) > tc.wantWithin {
				t.Errorf("Spawn took %v, want under %v", time.Since(start), tc.wantWithin)
			}
			if out.String() != tc.wantOut || errb.String() != tc.wantErr || exit != tc.wantExit {
				t.Errorf("Spawn = %q, %q, %d; want %q, %q, %d", out.String(), errb.String(), exit, tc.wantOut, tc.wantErr, tc.wantExit)
			}
		})
	}
}

// TestSpawnEndsWithItsContext pins the kill: a program still running when
// ctx ends is gone, its whole group with it, and the exit is -1.
func TestSpawnEndsWithItsContext(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the row runs sh")
	}
	target := spawnable(t, "sh")
	ctx, cancel := context.WithTimeout(t.Context(), 500*time.Millisecond)
	defer cancel()
	start := time.Now()
	exit, err := target.Spawn(ctx, []string{"-c", "sleep 30"}, nil, io.Discard, io.Discard)
	if err != nil {
		t.Fatalf("Spawn: %v", err)
	}
	if exit != -1 {
		t.Errorf("exit = %d, want -1 for a killed program", exit)
	}
	if time.Since(start) > 5*time.Second {
		t.Errorf("Spawn took %v after a 500ms deadline", time.Since(start))
	}
}

// TestSpawnStartFailureIsAnError pins the one error Spawn returns: the
// program could not be started at all.
func TestSpawnStartFailureIsAnError(t *testing.T) {
	target := &TargetImpl{ref: "nope", path: "/nonexistent/program", log: slog.New(slog.DiscardHandler)}
	if _, err := target.Spawn(t.Context(), nil, nil, io.Discard, io.Discard); err == nil {
		t.Fatal("Spawn of a missing program returned no error")
	}
}

// spawnBounded runs Spawn on a goroutine and gives it until within to
// return; past that it closes stdin to free a hung Wait and fails, so a
// regression is a red row rather than a hung suite.
func spawnBounded(t *testing.T, target *TargetImpl, ctx context.Context, argv []string, stdin *io.PipeReader, within time.Duration) int {
	t.Helper()
	type result struct {
		exit int
		err  error
	}
	done := make(chan result, 1)
	go func() {
		exit, err := target.Spawn(ctx, argv, stdin, io.Discard, io.Discard)
		done <- result{exit, err}
	}()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("Spawn: %v", r.err)
		}
		return r.exit
	case <-time.After(within):
		_ = stdin.Close()
		<-done
		t.Fatalf("Spawn did not return within %v of its program ending while stdin stayed open", within)
		return 0
	}
}

// TestSpawnDoesNotWaitOnAnOpenStdin pins what a session hands Spawn: a pipe
// that reaches EOF only when the session closes it. A program that exits on
// its own, or is ended by its context, is still reported promptly — neither
// can be held hostage by a reader nobody is writing to.
func TestSpawnDoesNotWaitOnAnOpenStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the rows run sh")
	}
	t.Run("the program exits on its own", func(t *testing.T) {
		pr, pw := io.Pipe()
		defer pw.Close()
		if exit := spawnBounded(t, spawnable(t, "sh"), t.Context(), []string{"-c", "exit 4"}, pr, pipeWait+3*time.Second); exit != 4 {
			t.Errorf("exit = %d, want 4", exit)
		}
	})
	t.Run("the context ends the program", func(t *testing.T) {
		pr, pw := io.Pipe()
		defer pw.Close()
		ctx, cancel := context.WithTimeout(t.Context(), 300*time.Millisecond)
		defer cancel()
		if exit := spawnBounded(t, spawnable(t, "sh"), ctx, []string{"-c", "sleep 30"}, pr, pipeWait+3*time.Second); exit != -1 {
			t.Errorf("exit = %d, want -1", exit)
		}
	})
	t.Run("what arrives on the pipe reaches the program", func(t *testing.T) {
		pr, pw := io.Pipe()
		go func() { _, _ = io.WriteString(pw, "exit 6\n") }()
		defer pw.Close()
		if exit := spawnBounded(t, spawnable(t, "sh"), t.Context(), nil, pr, pipeWait+3*time.Second); exit != 6 {
			t.Errorf("exit = %d, want the 6 the script said", exit)
		}
	})
}
