package mcp

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// echoSpawner is a process that echoes what reaches its stdin back with a
// prefix until stdin closes, then exits 5 — enough of a shell to pin that a
// session keeps one process across calls.
type echoSpawner struct{ started int }

func (e *echoSpawner) Spawn(ctx context.Context, _ []string, stdin io.Reader, stdout, _ io.Writer) (int, error) {
	e.started++
	buf := make([]byte, 4096)
	for {
		n, err := stdin.Read(buf)
		if n > 0 {
			_, _ = io.WriteString(stdout, "> "+string(buf[:n]))
		}
		if err != nil {
			return 5, nil
		}
		if ctx.Err() != nil {
			return -1, nil
		}
	}
}

// TestSessionKeepsOneProcess pins the point of a session: two writes reach
// the same process, reads return what arrived since the last, and closing
// ends it.
func TestSessionKeepsOneProcess(t *testing.T) {
	sp := &echoSpawner{}
	cs := connect(t, []Origin{{Kind: KindProgram, Spawner: sp}})
	var opened sessionOpenOut
	if msg := call(t, cs, "session_open", map[string]any{"n": 0}, &opened); msg != "" {
		t.Fatal(msg)
	}
	for _, line := range []string{"one\n", "two\n"} {
		if msg := call(t, cs, "session_write", map[string]any{"id": opened.ID, "stdin": line}, nil); msg != "" {
			t.Fatal(msg)
		}
		var got sessionReadOut
		if msg := call(t, cs, "session_read", map[string]any{"id": opened.ID, "timeout_ms": 2000}, &got); msg != "" {
			t.Fatal(msg)
		}
		if got.Stdout != "> "+line || got.Exited {
			t.Errorf("session_read after %q = %+v", line, got)
		}
	}
	var empty sessionReadOut
	if msg := call(t, cs, "session_read", map[string]any{"id": opened.ID, "timeout_ms": 50}, &empty); msg != "" || empty.Stdout != "" {
		t.Errorf("a read with nothing new = %+v, %q; want empty", empty, msg)
	}
	if msg := call(t, cs, "session_close", map[string]any{"id": opened.ID}, nil); msg != "" {
		t.Fatal(msg)
	}
	if sp.started != 1 {
		t.Errorf("the session started %d processes, want 1", sp.started)
	}
	if msg := call(t, cs, "session_read", map[string]any{"id": opened.ID}, nil); !strings.Contains(msg, "no session") {
		t.Errorf("a read after close = %q, want no session", msg)
	}
}

// TestSessionReportsTheExit pins what a read says once the process is gone:
// everything it printed, exited, with its code — and the session is then
// forgotten.
func TestSessionReportsTheExit(t *testing.T) {
	cs := connect(t, []Origin{{Kind: KindProgram, Spawner: &fakeSpawner{out: "bye\n", exit: 4}}})
	var opened sessionOpenOut
	if msg := call(t, cs, "session_open", map[string]any{"n": 0, "argv": []string{"true"}}, &opened); msg != "" {
		t.Fatal(msg)
	}
	var printed strings.Builder
	var got sessionReadOut
	deadline := time.Now().Add(5 * time.Second)
	for !got.Exited && time.Now().Before(deadline) {
		if msg := call(t, cs, "session_read", map[string]any{"id": opened.ID, "timeout_ms": 200}, &got); msg != "" {
			t.Fatal(msg)
		}
		printed.WriteString(got.Stdout)
	}
	if !got.Exited || got.ExitCode != 4 || printed.String() != "bye\n" {
		t.Errorf("session_read = %+v after %q, want exited with 4 after \"bye\\n\"", got, printed.String())
	}
	if msg := call(t, cs, "session_read", map[string]any{"id": opened.ID}, nil); !strings.Contains(msg, "no session") {
		t.Errorf("a read after the exit was reported = %q, want no session", msg)
	}
}

// TestSessionRefusals pins the refusals: an origin that cannot spawn, an id
// nobody opened, and a write to a process that has gone.
func TestSessionRefusals(t *testing.T) {
	cs := connect(t, []Origin{
		{Kind: KindHTTP},
		{Kind: KindProgram, Spawner: &fakeSpawner{exit: 2}},
	})
	if msg := call(t, cs, "session_open", map[string]any{"n": 0}, nil); msg != "origin 0 cannot run a program: it is an http origin" {
		t.Errorf("session_open on http = %q", msg)
	}
	if msg := call(t, cs, "session_write", map[string]any{"id": "nope", "stdin": "x"}, nil); !strings.Contains(msg, `no session "nope"`) {
		t.Errorf("session_write to an unknown id = %q", msg)
	}
	var opened sessionOpenOut
	if msg := call(t, cs, "session_open", map[string]any{"n": 1}, &opened); msg != "" {
		t.Fatal(msg)
	}
	deadline := time.Now().Add(5 * time.Second)
	var msg string
	for time.Now().Before(deadline) {
		if msg = call(t, cs, "session_write", map[string]any{"id": opened.ID, "stdin": "x\n"}, nil); msg != "" {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(msg, "exited with 2") {
		t.Errorf("session_write after the process exited = %q, want it to say so", msg)
	}
}

// TestSessionLimits pins the table's edges: the seventeenth open is refused,
// an idle session is reaped, and Close ends them all.
func TestSessionLimits(t *testing.T) {
	origins := []Origin{{Kind: KindProgram, Spawner: &fakeSpawner{block: true}}}
	tbl := newSessions(origins, slog.New(slog.DiscardHandler))
	defer tbl.Close()
	for i := 0; i < maxSessions; i++ {
		if _, _, err := tbl.open(t.Context(), nil, sessionOpenIn{N: 0}); err != nil {
			t.Fatalf("open %d: %v", i, err)
		}
	}
	if _, _, err := tbl.open(t.Context(), nil, sessionOpenIn{N: 0}); err == nil || !strings.Contains(err.Error(), "16 sessions") {
		t.Errorf("the 17th open = %v, want refused", err)
	}
	tbl.mu.Lock()
	for _, s := range tbl.table {
		s.last = time.Now().Add(-sessionIdle - time.Second)
	}
	tbl.mu.Unlock()
	tbl.reap()
	if n := tbl.count(); n != 0 {
		t.Errorf("%d sessions after a reap of idle ones, want 0", n)
	}
	if _, _, err := tbl.open(t.Context(), nil, sessionOpenIn{N: 0}); err != nil {
		t.Fatalf("open after the reap: %v", err)
	}
	done := make(chan struct{})
	go func() { _ = tbl.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end the sessions")
	}
	if n := tbl.count(); n != 0 {
		t.Errorf("%d sessions after Close, want 0", n)
	}
}
