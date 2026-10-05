package mcp

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestOrigins pins the list: index, the origin as shown, and the kind,
// whether or not it can spawn.
func TestOrigins(t *testing.T) {
	cs := connect(t, []Origin{
		{Name: "exec:///bin/sh", Kind: KindProgram, Spawner: &fakeSpawner{}},
		{Name: "attach://dockerd/web", Kind: KindContainer},
		{Name: "http://localhost:3000", Kind: KindHTTP},
	})
	var got []struct {
		N      int    `json:"n"`
		Origin string `json:"origin"`
		Kind   string `json:"kind"`
	}
	if msg := call(t, cs, "origins", nil, &got); msg != "" {
		t.Fatal(msg)
	}
	if len(got) != 3 || got[0].N != 0 || got[0].Origin != "exec:///bin/sh" || got[0].Kind != "program" ||
		got[1].Kind != "container" || got[2].N != 2 || got[2].Kind != "http" {
		t.Errorf("origins = %+v", got)
	}
}

// TestExec pins the one-shot command: streams apart, the exit code, stdin
// fed, output capped and marked, bad bytes replaced, and a refusal for an
// origin that cannot run anything.
func TestExec(t *testing.T) {
	for _, tc := range []struct {
		name    string
		origins []Origin
		args    map[string]any
		want    execOut
		wantMsg string
	}{
		{"streams and exit", []Origin{{Kind: KindProgram, Spawner: &fakeSpawner{out: "o", errText: "e", exit: 3}}},
			map[string]any{"n": 0, "argv": []string{"-c", "x"}}, execOut{Stdout: "o", Stderr: "e", ExitCode: 3}, ""},
		{"stdin is fed", []Origin{{Kind: KindProgram, Spawner: &fakeSpawner{echoStdin: true}}},
			map[string]any{"n": 0, "argv": []string{"cat"}, "stdin": "fed"}, execOut{Stdout: "fed"}, ""},
		{"one byte past the cap is dropped and marked", []Origin{{Kind: KindProgram, Spawner: &fakeSpawner{out: strings.Repeat("x", maxOutput+1)}}},
			map[string]any{"n": 0, "argv": []string{"big"}}, execOut{Stdout: strings.Repeat("x", maxOutput), Truncated: true}, ""},
		{"output past the cap is dropped and marked", []Origin{{Kind: KindProgram, Spawner: &fakeSpawner{out: strings.Repeat("x", maxOutput+5)}}},
			map[string]any{"n": 0, "argv": []string{"big"}}, execOut{Stdout: strings.Repeat("x", maxOutput), Truncated: true}, ""},
		{"non-UTF-8 output is replaced", []Origin{{Kind: KindProgram, Spawner: &fakeSpawner{out: "a\xffb"}}},
			map[string]any{"n": 0, "argv": []string{"bin"}}, execOut{Stdout: "a\uFFFDb"}, ""},
		{"an http origin refuses", []Origin{{Name: "http://localhost:3000", Kind: KindHTTP}},
			map[string]any{"n": 0, "argv": []string{"ls"}}, execOut{}, "origin 0 cannot run a program: it is an http origin"},
		{"a container with no spawner refuses", []Origin{{Name: "attach://dockerd/web", Kind: KindContainer}},
			map[string]any{"n": 0, "argv": []string{"ls"}}, execOut{}, "origin 0 cannot run a program: it is a container origin"},
		{"the index one past the last refuses", []Origin{{Kind: KindProgram, Spawner: &fakeSpawner{}}},
			map[string]any{"n": 1, "argv": []string{"ls"}}, execOut{}, "origin 1: there are 1 origins, 0 to 0"},
		{"an index off the list refuses", []Origin{{Kind: KindProgram, Spawner: &fakeSpawner{}}},
			map[string]any{"n": 4, "argv": []string{"ls"}}, execOut{}, "origin 4: there are 1 origins, 0 to 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cs := connect(t, tc.origins)
			var got execOut
			msg := call(t, cs, "exec", tc.args, &got)
			if msg != tc.wantMsg {
				t.Fatalf("error = %q, want %q", msg, tc.wantMsg)
			}
			if msg == "" && got != tc.want {
				t.Errorf("exec = %+v, want %+v", got, tc.want)
			}
		})
	}
}

// TestExecTimeout pins timeout_ms: a program that never returns is killed at
// the deadline with exit -1; zero or less means the default, and past the
// maximum is the maximum, so an agent that asks for an hour gets ten minutes
// rather than a refusal.
func TestExecTimeout(t *testing.T) {
	cs := connect(t, []Origin{{Kind: KindProgram, Spawner: &fakeSpawner{block: true}}})
	var got execOut
	if msg := call(t, cs, "exec", map[string]any{"n": 0, "argv": []string{"hang"}, "timeout_ms": 50}, &got); msg != "" {
		t.Fatal(msg)
	}
	if got.ExitCode != -1 {
		t.Errorf("a timed-out exec = %+v, want exit -1", got)
	}
	for in, want := range map[int]int{0: defaultTimeoutMs, -5: defaultTimeoutMs, maxTimeoutMs + 1: maxTimeoutMs, 1234: 1234} {
		if got := clampTimeout(in); got != want {
			t.Errorf("clampTimeout(%d) = %d, want %d", in, got, want)
		}
	}
}

// TestCapped pins the writer exec and sessions collect output in: every byte
// is accepted, whether kept or dropped, so the copy feeding it never stops
// short; and what it hands back is valid UTF-8 on its own, not by the grace
// of whichever JSON encoder carries it.
func TestCapped(t *testing.T) {
	c := newCapped(4)
	for _, p := range []string{"a\xffb", "cdef"} {
		if n, err := c.Write([]byte(p)); n != len(p) || err != nil {
			t.Errorf("Write(%q) = %d, %v; want %d, nil", p, n, err, len(p))
		}
	}
	if got := c.text(); got != "a\uFFFDbc" || !c.truncated {
		t.Errorf("text() = %q, truncated %v; want %q, true", got, c.truncated, "a\uFFFDbc")
	}
	if got := c.take(); got != "a\uFFFDbc" || c.truncated || c.text() != "" {
		t.Errorf("take() = %q, then truncated %v and %q left; want everything once", got, c.truncated, c.text())
	}
}

// heldSpawner runs until its context ends, says when it has started, and
// takes a moment to go once told, as a real process does past its signal.
// A second call returns at once: only the first is held.
type heldSpawner struct {
	started, gone chan struct{}
	calls         atomic.Int32
}

func (h *heldSpawner) Spawn(ctx context.Context, _ []string, _ io.Reader, _, _ io.Writer) (int, error) {
	if h.calls.Add(1) > 1 {
		return 0, nil
	}
	close(h.started)
	<-ctx.Done()
	time.Sleep(100 * time.Millisecond)
	close(h.gone)
	return -1, nil
}

// TestCloseEndsAnExec pins that closing the handler ends a command still
// running and returns only once it has gone, whatever its timeout: nothing
// an agent started outlives the run.
func TestCloseEndsAnExec(t *testing.T) {
	sp := &heldSpawner{started: make(chan struct{}), gone: make(chan struct{})}
	server, closer := newServer([]Origin{{Kind: KindProgram, Spawner: sp}}, slog.New(slog.DiscardHandler))
	ct, st := sdk.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer ss.Close()
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.Close()
	go func() {
		_, _ = cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "exec", Arguments: map[string]any{"n": 0, "argv": []string{"hang"}, "timeout_ms": maxTimeoutMs}})
	}()
	select {
	case <-sp.started:
	case <-time.After(5 * time.Second):
		t.Fatal("the exec never started")
	}
	done := make(chan struct{})
	go func() { _ = closer.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not return")
	}
	select {
	case <-sp.gone:
	default:
		t.Error("Close returned while the exec's process was still going")
	}
	var refused string
	if res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: "exec", Arguments: map[string]any{"n": 0, "argv": []string{"x"}}}); err == nil && res.IsError {
		refused = res.Content[0].(*sdk.TextContent).Text
	}
	if refused != "the run is ending" {
		t.Errorf("an exec after Close = %q, want it refused", refused)
	}
}

// killedWithOne is a program the platform reports as exiting 1 when it is
// killed, as TerminateProcess does on Windows.
type killedWithOne struct{}

func (killedWithOne) Spawn(ctx context.Context, _ []string, _ io.Reader, _, _ io.Writer) (int, error) {
	<-ctx.Done()
	return 1, nil
}

// TestExecTimeoutIsMinusOneEverywhere pins the timeout's exit code to the
// tool rather than the platform: an agent can tell a timeout from a program
// that exited 1 on any machine tunneld runs on.
func TestExecTimeoutIsMinusOneEverywhere(t *testing.T) {
	cs := connect(t, []Origin{{Kind: KindProgram, Spawner: killedWithOne{}}})
	var got execOut
	if msg := call(t, cs, "exec", map[string]any{"n": 0, "argv": []string{"hang"}, "timeout_ms": 50}, &got); msg != "" {
		t.Fatal(msg)
	}
	if got.ExitCode != -1 {
		t.Errorf("a timed-out exec = %+v, want exit -1 whatever the platform said", got)
	}
}
