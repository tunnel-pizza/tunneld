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
