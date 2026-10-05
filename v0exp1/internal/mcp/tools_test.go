package mcp

import (
	"strings"
	"testing"
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
