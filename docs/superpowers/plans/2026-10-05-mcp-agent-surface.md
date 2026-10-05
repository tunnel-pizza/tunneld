# MCP Agent Surface Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** One MCP server per run, served by tunneld at `/_tunneld/mcp` behind the tunnel secret, through which an agent runs commands, moves files and holds a private shell on the run's program origins.

**Architecture:** An experiment, `v0exp1.Experimental().Mcp()`, builds a streamable-HTTP MCP handler over the run's origins; the builder mounts it on the router's control path with a new `router.WithHandler(pattern, h)`. Tools name an origin by index and spawn private processes through a new `attach.Spawner` primitive that the shell provider implements. The human page and the shared `/attach` are untouched.

**Tech Stack:** Go 1.26, `github.com/modelcontextprotocol/go-sdk/mcp` v1.8.0 (new direct dependency), `os/exec`, the existing `attach`, `shell`, `router` packages.

**Spec:** `docs/superpowers/specs/2026-10-05-mcp-agent-surface-design.md`

## Global Constraints

- Go floor stays `go 1.26.0` in `go.mod`; the SDK is pinned at `v1.8.0`.
- The SDK is imported by `v0exp1/internal/mcp` only, aliased `sdk` (the package is itself named `mcp`).
- One test file per source file: `x.go` → `x_test.go`, new cases join the table in the existing file. `README.md`'s test is `readme_test.go`.
- A new direct dependency needs a line in README `## Acknowledgements` (`readme_test.go` enforces both directions).
- `main.go` is not touched.
- Commit messages: `type(scope): subject`, body in sentences, ending with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`. Every commit is gated on `make fmt-check test` passing (`ℹ fail 0`). Before any push, `go test -short ./e2e` and `GOOS=linux go vet ./...` plus `GOOS=windows go vet ./...`.
- Limits, verbatim from the spec: stdout and stderr each 1 MiB; `timeout_ms` default 60 000, max 600 000; files 16 MiB; 16 sessions per run; idle session reaped after 10 min; `session_read` default wait 1 000 ms.
- Nothing under `/_tunneld/` is reachable without `Authorization: token <base64 secret>`; this plan adds no auth code.
- `HANDOFF-agent-ux.md` at the repo root is never committed.

## Review Focus

1. **A request through the tunnel carries the public hostname as `Host` while the router listens on 127.0.0.1.** The SDK's handler rejects that as DNS rebinding with 403 unless `DisableLocalhostProtection` is set. Expected: 200. Pinned in Task 5, `TestHandlerAcceptsAForwardedHost`.
2. **A program prints bytes that are not UTF-8** (a binary, a Latin-1 file). JSON cannot carry them and the SDK would fail the response. Expected: the output arrives with the bad bytes replaced by U+FFFD. Pinned in Task 5, `TestExec` row "non-UTF-8 output is replaced".
3. **A command starts a background child that keeps stdout open** (`sleep 30 & echo hi`). Expected: `exec` returns when the shell exits, within `pipeWait` (2 s), not when the child does. Pinned in Task 4, `TestSpawn` row "a background child does not hold the result".
4. **`timeout_ms` is 0, negative, or above the maximum.** Expected: 0 and negative mean the default (60 000); above the maximum is clamped to 600 000, not refused. Pinned in Task 5, `TestExecTimeout`.
5. **`put_file` is given a relative path, or a path that is a directory.** Expected: refused with a one-line reason as a tool error, nothing written. Pinned in Task 6, `TestFiles` rows "a relative path is refused" and "a directory is refused".

---

### Task 1: The dependency and its credit

**Files:**
- Modify: `go.mod`, `go.sum`
- Modify: `README.md` (the `## Acknowledgements` list, after the u-root entry)
- Test: `readme_test.go` (existing; no change)

**Interfaces:**
- Produces: `github.com/modelcontextprotocol/go-sdk/mcp` importable at v1.8.0.

- [ ] **Step 1: Add the module**

```sh
go get github.com/modelcontextprotocol/go-sdk@v1.8.0
```

It lands as `// indirect` until something imports it. That is expected; Task 5 makes it direct, and `go mod tidy` there will drop the comment.

- [ ] **Step 2: Run the README test to see it fail**

Run: `go test ./ -run TestAcknowledgements -v`
Expected: PASS for now (an indirect requirement is not credited). Keep going; this step is here so the executor knows the test exists and will bite in Task 5.

- [ ] **Step 3: Add the credit**

In `README.md`, under `## Acknowledgements`, after the u-root bullet (the one beginning `- **[u-root](https://github.com/u-root/u-root)**`), add:

```markdown
- **[MCP Go SDK](https://github.com/modelcontextprotocol/go-sdk)**
  (`github.com/modelcontextprotocol/go-sdk`): the Model Context Protocol
  server tunneld serves to agents on its control path.
```

- [ ] **Step 4: Verify the README test still passes**

Run: `go test ./ -run TestAcknowledgements -v`
Expected: PASS. (`TestAcknowledgementsCreditOnlyWhatIsRequired` accepts an indirect requirement.)

- [ ] **Step 5: Commit**

```sh
git add go.mod go.sum README.md
git commit -m "build: take the MCP Go SDK as a dependency (#243)

Pinned at v1.8.0 and credited in the README's Acknowledgements. Indirect
until the server that imports it lands.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: `router.WithWrap` and a control-path mount

**Files:**
- Modify: `v1alpha1/router/router.go` (`WithHandler` at ~211; `RouterImpl` fields ~100-126; `Handler()` accessor ~334; `Route` ~404-450)
- Modify: `v1alpha1/builder.go:645` (the one call site)
- Modify: `v1alpha1/builder_test.go:738-780` (`fakeRouter` reads `r.Handler()`)
- Test: `v1alpha1/router/router_test.go` (`route` helper at 60-70 uses `WithHandler(front)`; `TestRouteAppliesTheFront` at 620)

**Interfaces:**
- Consumes: nothing new.
- Produces:
  - `func WithWrap(wrap func(http.Handler) http.Handler) Option` (the old `WithHandler`).
  - `func WithHandler(pattern string, h http.Handler) Option`.
  - `func (r *RouterImpl) Wrap() func(http.Handler) http.Handler` (the old `Handler()` accessor).
  - `func (r *RouterImpl) Mounted() []string` — the patterns `WithHandler` put on the mux, in the order given.
  - `Route` returns `error` wrapping the message `router: a handler at %q is outside the control path` when any mounted pattern lacks the `ControlPath` prefix.

- [ ] **Step 1: Rename the wrap option everywhere**

In `v1alpha1/router/router.go`:

```go
// WithWrap sets what wraps the routing handler — the display's Panel — or
// nothing, when nil.
func WithWrap(wrap func(http.Handler) http.Handler) Option {
	return func(r *RouterImpl) { r.wrap = wrap }
}
```

Rename the field `handler` on `RouterImpl` to `wrap` (its comment stays), the accessor `Handler()` to `Wrap()`, and in `Route` the local `front := route.handler` to `route.wrap`.

In `v1alpha1/builder.go:645`: `router.WithWrap(b.display.Panel(b.multiview, origins, log)),`

In `v1alpha1/builder_test.go` `fakeRouter.Route`: `dialable, front := r.Origins(), r.Wrap()`.

In `v1alpha1/router/router_test.go` `route` helper: `WithWrap(front)`.

- [ ] **Step 2: Build and run the router and builder tests to confirm the rename is whole**

Run: `go build ./... && go test ./v1alpha1/router ./v1alpha1 -count=1`
Expected: PASS.

- [ ] **Step 3: Write the failing tests for the mount**

Append to `v1alpha1/router/router_test.go`:

```go
// TestWithHandlerMountsOnTheControlPath pins the mount: a handler put under
// ControlPath answers there, through authorize — a bare 401 without the
// secret, the handler's own answer with it — and Mounted reads the pattern
// back without standing anything up.
func TestWithHandlerMountsOnTheControlPath(t *testing.T) {
	secret := []byte("s3cr3t")
	hello := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { fmt.Fprint(w, "hello") })
	r := New(WithHandler(ControlPath+"hello", hello), WithCache(cacheOf{secret: secret, key: runKey}))
	if got, want := r.Mounted(), []string{ControlPath + "hello"}; !slices.Equal(got, want) {
		t.Errorf("Mounted() = %v, want %v", got, want)
	}
	u, err := r.Route(t.Context(), WithOrigins(listOf(t, echo(t, "solo"))))
	if err != nil {
		t.Fatalf("Route: %v", err)
	}
	t.Cleanup(r.Cancel)
	base := strings.TrimSuffix(u.String(), "/")
	token := tokenOf(secret)
	for name, tc := range map[string]struct {
		auth       string
		wantStatus int
		wantBody   string
	}{
		"without the secret": {"", 401, ""},
		"with the secret":    {token, 200, "hello"},
	} {
		t.Run(name, func(t *testing.T) {
			req, _ := http.NewRequest("GET", base+ControlPath+"hello", nil)
			if tc.auth != "" {
				req.Header.Set("Authorization", tc.auth)
			}
			resp, body := get(t, http.DefaultClient, req)
			if resp.StatusCode != tc.wantStatus || body != tc.wantBody {
				t.Errorf("GET = %d %q, want %d %q", resp.StatusCode, body, tc.wantStatus, tc.wantBody)
			}
		})
	}
}

// TestWithHandlerRefusesAnOriginsPath pins that a pattern outside ControlPath
// is an error when the router routes: every other path is an origin's, and a
// handler there would shadow it silently.
func TestWithHandlerRefusesAnOriginsPath(t *testing.T) {
	r := New(WithHandler("/mcp", http.NotFoundHandler()))
	_, err := r.Route(t.Context(), WithOrigins(listOf(t, echo(t, "solo"))))
	if err == nil || !strings.Contains(err.Error(), `a handler at "/mcp" is outside the control path`) {
		t.Fatalf("Route = %v, want the pattern refused", err)
	}
}
```

`cacheOf`, `runKey` and `tokenOf` already exist in `router_test.go` (~235-260). Add `"slices"` to the test's imports if absent, and to `router.go`'s for `Mounted`.

- [ ] **Step 4: Run the tests to see them fail**

Run: `go test ./v1alpha1/router -run 'TestWithHandler' -v`
Expected: FAIL to compile — `undefined: WithHandler` taking two arguments, `undefined: Mounted`.

- [ ] **Step 5: Implement the mount**

In `router.go`, add to `RouterImpl` (beside `mux`):

```go
	// mounted is every pattern WithHandler put on the mux, in order, and
	// mountErr the first pattern that was not the control path's: an
	// option cannot refuse, so Route does, with this.
	mounted  []string
	mountErr error
```

Add the option after `WithWrap`:

```go
// WithHandler puts h on the router's mux at pattern, which must be under
// ControlPath: that prefix is the mux's and authorize's, and every other
// path is an origin's. The same pattern twice keeps the first; a pattern
// outside the control path is refused when the router routes, since an
// option has no error to return.
func WithHandler(pattern string, h http.Handler) Option {
	return func(r *RouterImpl) {
		if !strings.HasPrefix(pattern, ControlPath) {
			if r.mountErr == nil {
				r.mountErr = fmt.Errorf("router: a handler at %q is outside the control path", pattern)
			}
			return
		}
		e := r.env
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.registered[pattern] {
			return
		}
		if e.registered == nil {
			e.registered = map[string]bool{}
		}
		e.registered[pattern] = true
		r.mux.Handle(pattern, h)
		r.mounted = append(r.mounted, pattern)
	}
}
```

Add the accessor beside `Wrap()`:

```go
func (r *RouterImpl) Mounted() []string { return slices.Clone(r.mounted) }
```

In `Route`, right after `v1.Apply(&route, opts...)`:

```go
	if route.mountErr != nil {
		return nil, route.mountErr
	}
```

`registered` lives on `env` (a pointer shared by copies), so a pattern mounted on the router before `Route` copies it is still seen as registered by the copy. `mounted` is a slice on the value; `Route`'s copy appending to it is fine because `Mounted` is read off whichever `RouterImpl` the caller holds.

- [ ] **Step 6: Run the tests to see them pass**

Run: `go test ./v1alpha1/router -count=1 -v -run 'TestWithHandler|TestControlPath|TestRouteAppliesTheFront'`
Expected: PASS.

- [ ] **Step 7: Full gate and commit**

Run: `make fmt-check test`
Expected: `ℹ fail 0`.

```sh
git add v1alpha1/router/router.go v1alpha1/router/router_test.go v1alpha1/builder.go v1alpha1/builder_test.go
git commit -m "feat(router): WithHandler mounts a handler on the control path; the wrap is WithWrap (#243)

The name goes to the mount because that is what a caller reads it as. A
pattern outside ControlPath is refused when the router routes, and
Mounted reads the patterns back for a test that stands nothing up.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: `attach.Spawner` and `Bound.Spawners`

**Files:**
- Modify: `v1alpha1/attach/attach.go` (`Target` ~68-110; `Bind` 352-415; `Bound` 438; `bound`/`boundOrigin` 451, 520; `sole` 420)
- Modify: `v1alpha1/builder_test.go:666-730` (`fakeBinder`)
- Modify: `v1alpha1/console/console_test.go:41,218` (`fakeOrigin`, `noOrigin`)
- Test: `v1alpha1/attach/attach_test.go` (beside `TestAnnounceReachesTheRightTerminal` at 1577; `stubTargets` is there)

**Interfaces:**
- Produces:
  ```go
  // Spawner starts one private process on an origin, over pipes, and reports
  // how it ended: the exit code, -1 when a signal ended it. The process
  // ends with ctx. err is a failure to start, never a non-zero exit.
  type Spawner interface {
      Spawn(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (exit int, err error)
  }
  ```
  and on `Bound`: `Spawners() []Spawner` — same length and order as the shown origins, nil where origin n cannot spawn.

- [ ] **Step 1: Write the failing test**

Append to `v1alpha1/attach/attach_test.go`:

```go
// spawningTarget is a stub target that can also spawn, for the one thing
// Bind has to get right about it: which index it lands on.
type spawningTarget struct {
	Target
	spawned [][]string
}

func (s *spawningTarget) Spawn(_ context.Context, argv []string, _ io.Reader, _, _ io.Writer) (int, error) {
	s.spawned = append(s.spawned, argv)
	return 7, nil
}

// TestSpawnersKeepTheOriginsOrder pins that Bound.Spawners has one slot per
// shown origin, nil for an address and for a target that cannot spawn, and
// the target's own spawner where it can — at the index the origin had.
// spawningTargets is stubTargets with one reference, db, opened as a target
// that can spawn.
type spawningTargets struct {
	*stubTargets
	can *spawningTarget
}

func (s *spawningTargets) Open(ctx context.Context, ref string, args []string, log *slog.Logger) (Target, error) {
	target, err := s.stubTargets.Open(ctx, ref, args, log)
	if err != nil || ref != "db" {
		return target, err
	}
	s.can.Target = target
	return s.can, nil
}

func TestSpawnersKeepTheOriginsOrder(t *testing.T) {
	can := &spawningTarget{}
	targets := &spawningTargets{stubTargets: &stubTargets{}, can: can}
	display := shown(t, "http://localhost:3000", "attach://dockerd/api", "attach://dockerd/db")
	_, closer, err := New(WithTargets(targets)).Bind(t.Context(), display, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatalf("Bind: %v", err)
	}
	defer closer.Close()
	got := closer.Spawners()
	if len(got) != 3 || got[0] != nil || got[1] != nil || got[2] == nil {
		t.Fatalf("Spawners() = %v, want [nil nil spawner]", got)
	}
	if exit, _ := got[2].Spawn(t.Context(), []string{"true"}, nil, io.Discard, io.Discard); exit != 7 {
		t.Errorf("Spawn through Bound = %d, want the target's 7", exit)
	}
}
```

`stubTargets` (attach_test.go ~1097) has `Open`, `Verb` and `Provider` on a pointer receiver, so embedding the pointer promotes the pair and lets `Open` be overridden as above. Run `go vet ./v1alpha1/attach` if `WithTargets` refuses the wrapper: it keys providers by `Verb()`/`Provider()`, which the embedding supplies.

- [ ] **Step 2: Run the test to see it fail**

Run: `go test ./v1alpha1/attach -run TestSpawnersKeepTheOriginsOrder -v`
Expected: FAIL to compile — `closer.Spawners undefined`.

- [ ] **Step 3: Implement**

In `attach.go`, after the `Target` interface:

```go
// Spawner is what a Target can do beyond being attached to: start one
// private process on the origin, over pipes — no terminal, no echo, no
// viewer — and say how it ended. It is what an agent's one-shot command
// runs on, beside the shared terminal a person is watching, and a provider
// that cannot do it simply does not implement it.
//
// exit is the process's exit code, -1 when a signal ended it; err is a
// failure to start, never a non-zero exit. The process ends with ctx.
type Spawner interface {
	Spawn(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (exit int, err error)
}
```

On `Bound`, add a method with its reason:

```go
type Bound interface {
	io.Closer
	Announce(public []string)
	Done() <-chan struct{}
	// Spawners is one slot per shown origin, in order: the origin's Spawner
	// where its target has one, nil for an address and for a target that
	// cannot spawn. Unconditional — every bound list can answer — which is
	// why it is here and not a type assertion.
	Spawners() []Spawner
}
```

In `boundOrigin`, add `spawner Spawner`; in `Bind`, when appending: 

```go
		spawner, _ := target.(Spawner)
		servers = append(servers, boundOrigin{at: at, srv: server, spawner: spawner})
```

and keep `shown.Len()` for the slice length. Add a field on `bound`? `bound` is a slice; the shown length is needed, so change `bound` to carry it:

```go
type bound struct {
	origins []boundOrigin
	// shown is how many origins the run has, served or not: the length of
	// what Spawners answers, since index n is origin n there too.
	shown int
}
```

and update every method on `bound` (`show`, `Done`, `Close`, `Announce`) and `Bind`'s `servers` and `sole` to the struct form — `servers.origins = append(...)`, `len(b.origins)`, `b.origins[0]`, and in `Bind` set `servers.shown = shown.Len()` before the loop. Then:

```go
// Spawners is one slot per shown origin, the served ones' spawners at the
// index each origin had.
func (b bound) Spawners() []Spawner {
	out := make([]Spawner, b.shown)
	for _, o := range b.origins {
		if o.spawner != nil {
			out[o.at] = o.spawner
		}
	}
	return out
}
```

`TestAnnounceReachesTheRightTerminal` asserts `closer.(bound)` and indexes it; update that assertion to `.origins`.

Fakes: in `v1alpha1/builder_test.go` add `func (f *fakeBinder) Spawners() []attach.Spawner { return f.spawners }` with a `spawners []attach.Spawner` field on `fakeBinder`; in `v1alpha1/console/console_test.go` add `func (f *fakeOrigin) Spawners() []attach.Spawner { return nil }` and `func (noOrigin) Spawners() []attach.Spawner { return nil }`.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./v1alpha1/attach ./v1alpha1/console ./v1alpha1 -count=1`
Expected: PASS.

- [ ] **Step 5: Full gate and commit**

Run: `make fmt-check test`
Expected: `ℹ fail 0`.

```sh
git add v1alpha1/attach/attach.go v1alpha1/attach/attach_test.go v1alpha1/builder_test.go v1alpha1/console/console_test.go
git commit -m "feat(attach): Spawner, and Bound.Spawners by origin index (#243)

A target that can start a private process says so by implementing
Spawner; Bind keeps each one at its origin's index so a caller reaches
origin n's the way it reaches everything else about origin n.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: The shell provider spawns

**Files:**
- Create: `v1alpha1/attach/shell/spawn.go`
- Create: `v1alpha1/attach/shell/spawn_test.go`
- Modify: `v1alpha1/attach/shell/shell.go:36-40` (the assertion block)

**Interfaces:**
- Consumes: `attach.Spawner` (Task 3); `pipeWait`, `ownGroup`, `hangup`, `kill` from this package.
- Produces: `func (a *TargetImpl) Spawn(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error)`. `argv == nil` runs the origin's own arguments (`a.args`), with no `-i`: a shell reading a pipe is what an agent wants.

- [ ] **Step 1: Write the failing test**

`v1alpha1/attach/shell/spawn_test.go`:

```go
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
		{"a background child does not hold the result", []string{"-c", "sleep 30 & echo hi"}, "", "hi\n", "", 0, pipeWait + 3*time.Second},
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
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./v1alpha1/attach/shell -run 'TestSpawn' -v`
Expected: FAIL to compile — `target.Spawn undefined`.

- [ ] **Step 3: Implement `spawn.go`**

```go
package shell

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
)

var _ attach.Spawner = (*TargetImpl)(nil)

// Spawn implements attach.Spawner: the program once more, privately, over
// pipes — the way an agent wants it, with no terminal to echo and no viewer
// to share with. argv replaces the origin's arguments; nil runs the origin
// as it was typed, and with no -i, since a shell reading a pipe as a script
// is the point here rather than a thing to work around.
//
// The exit code is the program's, -1 when a signal ended it, which is what
// a deadline does: ctx ending kills the whole group, the way End kills the
// attached program's, and the program's own stragglers are not waited on
// past pipeWait.
func (a *TargetImpl) Spawn(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	// Empty and nil alike: JSON hands an omitted list over as either.
	if len(argv) == 0 {
		argv = a.args
	}
	cmd := exec.CommandContext(ctx, a.path, argv...)
	cmd.Env = append(os.Environ(), "TERM=dumb")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = stdin, stdout, stderr
	cmd.WaitDelay = pipeWait
	ownGroup(cmd)
	cmd.Cancel = func() error {
		_ = hangup(cmd.Process)
		return kill(cmd.Process)
	}
	if err := cmd.Start(); err != nil {
		return -1, err
	}
	err := cmd.Wait()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		return ee.ExitCode(), nil
	default:
		// Wait's own failures past the exit — a pipe copy that WaitDelay
		// ended — are not the program's; what it reported stands.
		if cmd.ProcessState != nil {
			return cmd.ProcessState.ExitCode(), nil
		}
		return -1, err
	}
}
```

Add `_ attach.Spawner = (*TargetImpl)(nil)` to the assertion block in `shell.go` instead of the file-local one above if the block's comment says that is where they belong (it does; keep it there and drop the one in `spawn.go`).

On Windows `hangup` and `kill` exist (`signal_windows.go`) so this compiles on both; run `GOOS=windows go vet ./v1alpha1/attach/shell` to confirm.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./v1alpha1/attach/shell -run 'TestSpawn' -count=1 -v`
Expected: PASS, the background-child row within ~2 s.

- [ ] **Step 5: Full gate and commit**

Run: `make fmt-check test && GOOS=windows go vet ./v1alpha1/attach/shell`
Expected: `ℹ fail 0`, vet clean.

```sh
git add v1alpha1/attach/shell/spawn.go v1alpha1/attach/shell/spawn_test.go v1alpha1/attach/shell/shell.go
git commit -m "feat(shell): Spawn runs the program privately over pipes (#243)

The attach.Spawner for a local program: its own group, TERM=dumb, the
real exit code, killed with its context and not held by what it left in
the background.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: The server, `origins` and `exec`

**Files:**
- Create: `v0exp1/internal/mcp/mcp.go` — types, the server, `Handler`
- Create: `v0exp1/internal/mcp/mcp_test.go`
- Create: `v0exp1/internal/mcp/tools.go` — `origins`, `exec`, the capped writer
- Create: `v0exp1/internal/mcp/tools_test.go`
- Modify: `go.mod` (`go mod tidy` makes the SDK direct)

**Interfaces:**
- Consumes: nothing from `v1alpha1` — this package declares its own `Spawner` (identical shape to `attach.Spawner`, so any `attach.Spawner` satisfies it).
- Produces:
  ```go
  type Kind string
  const (
      KindProgram   Kind = "program"
      KindContainer Kind = "container"
      KindHTTP      Kind = "http"
  )
  type Spawner interface {
      Spawn(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (exit int, err error)
  }
  type Origin struct {
      Name    string
      Kind    Kind
      Spawner Spawner
  }
  func Handler(origins []Origin, log *slog.Logger) (http.Handler, io.Closer)
  func newServer(origins []Origin, log *slog.Logger) (*sdk.Server, *sessions) // sessions arrives in Task 7; until then return (*sdk.Server, io.Closer) with a no-op closer
  ```
  Tool names and shapes, as the spec's table: `origins`, `exec`.

- [ ] **Step 1: Write the failing tests**

`v0exp1/internal/mcp/mcp_test.go`:

```go
package mcp

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fakeSpawner is a Spawner that answers from a table: what it writes and
// how it exits, by the first word of argv. It records what it was asked.
type fakeSpawner struct {
	out, errText string
	exit         int
	echoStdin    bool
	block        bool // never returns until ctx ends
	argv         [][]string
}

func (f *fakeSpawner) Spawn(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	f.argv = append(f.argv, argv)
	if f.block {
		<-ctx.Done()
		return -1, nil
	}
	if f.echoStdin && stdin != nil {
		_, _ = io.Copy(stdout, stdin)
	}
	_, _ = io.WriteString(stdout, f.out)
	_, _ = io.WriteString(stderr, f.errText)
	return f.exit, nil
}

// connect stands the server up over an in-memory transport and returns a
// client session on it.
func connect(t *testing.T, origins []Origin) *sdk.ClientSession {
	t.Helper()
	server, closer := newServer(origins, slog.New(slog.DiscardHandler))
	t.Cleanup(func() { _ = closer.Close() })
	ct, st := sdk.NewInMemoryTransports()
	ss, err := server.Connect(t.Context(), st, nil)
	if err != nil {
		t.Fatalf("server.Connect: %v", err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil).Connect(t.Context(), ct, nil)
	if err != nil {
		t.Fatalf("client.Connect: %v", err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

// call runs one tool and decodes its structured result into out; it returns
// the tool error's text, "" for none.
func call(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any, out any) string {
	t.Helper()
	res, err := cs.CallTool(t.Context(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("CallTool %s: %v", name, err)
	}
	if res.IsError {
		var sb strings.Builder
		for _, c := range res.Content {
			if tc, ok := c.(*sdk.TextContent); ok {
				sb.WriteString(tc.Text)
			}
		}
		return sb.String()
	}
	if out != nil {
		raw, err := json.Marshal(res.StructuredContent)
		if err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(raw, out); err != nil {
			t.Fatalf("decoding %s result %s: %v", name, raw, err)
		}
	}
	return ""
}

// TestListsEveryTool pins the surface by name: what an agent sees in
// tools/list is the spec's table, nothing more.
func TestListsEveryTool(t *testing.T) {
	cs := connect(t, nil)
	res, err := cs.ListTools(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, tool := range res.Tools {
		got = append(got, tool.Name)
	}
	want := "exec get_file origins put_file session_close session_open session_read session_write"
	if strings.Join(sortedCopy(got), " ") != want {
		t.Errorf("tools = %v, want %s", got, want)
	}
}

// TestHandlerAcceptsAForwardedHost pins the loopback: the router listens on
// 127.0.0.1 and the tunnel forwards the public hostname as Host, which the
// SDK calls DNS rebinding and refuses unless told the secret is the guard.
func TestHandlerAcceptsAForwardedHost(t *testing.T) {
	h, closer := Handler(nil, slog.New(slog.DiscardHandler))
	defer closer.Close()
	srv := httptest.NewServer(h)
	defer srv.Close()
	body := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18","capabilities":{},"clientInfo":{"name":"t","version":"0"}}}`
	req, _ := http.NewRequest("POST", srv.URL, strings.NewReader(body))
	req.Host = "abc.tunnel.pizza"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("initialize with a forwarded Host = %d %s, want 200", resp.StatusCode, b)
	}
}
```

Write `sortedCopy` at the bottom of the file (`slices.Sorted(slices.Values(got))` is enough; name it to match).

Until Task 7 lands, `TestListsEveryTool`'s `want` would fail on the four session tools. Register them in this task as stubs that return the tool error `"sessions are not available yet"` so the surface is whole from the first commit, and Task 7 replaces the stubs.

`v0exp1/internal/mcp/tools_test.go`:

```go
package mcp

import (
	"strings"
	"testing"
)

// execOut is the tool's own result type, decoded back through its JSON tags.

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
		{"output past the cap is dropped and marked", []Origin{{Kind: KindProgram, Spawner: &fakeSpawner{out: strings.Repeat("x", maxOutput+5)}}},
			map[string]any{"n": 0, "argv": []string{"big"}}, execOut{Stdout: strings.Repeat("x", maxOutput), Truncated: true}, ""},
		{"non-UTF-8 output is replaced", []Origin{{Kind: KindProgram, Spawner: &fakeSpawner{out: "a\xffb"}}},
			map[string]any{"n": 0, "argv": []string{"bin"}}, execOut{Stdout: "a�b"}, ""},
		{"an http origin refuses", []Origin{{Name: "http://localhost:3000", Kind: KindHTTP}},
			map[string]any{"n": 0, "argv": []string{"ls"}}, execOut{}, "origin 0 cannot run a program: it is an http origin"},
		{"a container with no spawner refuses", []Origin{{Name: "attach://dockerd/web", Kind: KindContainer}},
			map[string]any{"n": 0, "argv": []string{"ls"}}, execOut{}, "origin 0 cannot run a program: it is a container origin"},
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

// TestExecTimeout pins timeout_ms: a program that never returns is killed
// at the deadline with exit -1; 0 means the default and past the maximum
// is the maximum, so an agent that asks for an hour gets ten minutes rather
// than a refusal.
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
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./v0exp1/internal/mcp -v`
Expected: FAIL to compile — package does not exist.

- [ ] **Step 3: Implement `mcp.go`**

```go
// Package mcp is the MCP server tunneld serves to agents: one per run, over
// streamable HTTP on the control path, knowing every origin of the run by
// the index the routing parameter and the stderr map use. The tools spawn
// private processes through each origin's Spawner, so an agent's command
// runs beside the terminal a person is watching rather than in it.
//
// The types a caller hands in are declared here and aliased by v0exp1,
// which is the only package allowed to import this one.
package mcp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Kind is what an origin is, which decides what the tools can do with it.
type Kind string

const (
	KindProgram   Kind = "program"   // exec://: a program on this machine
	KindContainer Kind = "container" // attach://: a container
	KindHTTP      Kind = "http"      // an address the tunnel dials
)

// Spawner starts one private process on an origin, over pipes, and reports
// how it ended: the exit code, -1 when a signal ended it. err is a failure
// to start, never a non-zero exit. The process ends with ctx. The same
// shape as attach.Spawner, declared here so this package imports nothing
// of v1alpha1's.
type Spawner interface {
	Spawn(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (exit int, err error)
}

// Origin is what the server knows about one origin of the run.
type Origin struct {
	Name    string  // the origin as shown, e.g. exec:///bin/sh
	Kind    Kind    // what it is
	Spawner Spawner // nil when the origin cannot start a process
}

// server is one run's tools over its origins.
type server struct {
	origins  []Origin
	log      *slog.Logger
	sessions *sessions
}

// Handler answers the MCP endpoint for these origins, index n being origin
// n, logging each call on log. Closing stops every session and process it
// started.
//
// Stateless: the SDK's own session id is not used — tool sessions have ids
// of their own — and a request's end cancels the call it carried, so a
// client that drops kills the process it was waiting on. Localhost
// protection is off because it would refuse exactly the shape every
// request here has: the router listens on 127.0.0.1 and the tunnel forwards
// the public hostname as Host. The secret in front of this handler is the
// guard, not the address.
func Handler(origins []Origin, log *slog.Logger) (http.Handler, io.Closer) {
	srv, closer := newServer(origins, log)
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, &sdk.StreamableHTTPOptions{
		Stateless:                    true,
		DisableLocalhostProtection:   true,
		PropagateRequestCancellation: true,
		// A put_file of maxFile arrives base64-encoded, four thirds the
		// size, inside a JSON envelope.
		MaxRequestBodyBytes: maxFile*4/3 + 1<<20,
	})
	return h, closer
}

// newServer builds the SDK server with every tool registered. The closer
// ends the sessions.
func newServer(origins []Origin, log *slog.Logger) (*sdk.Server, io.Closer) {
	s := &server{origins: origins, log: log, sessions: newSessions(origins, log)}
	srv := sdk.NewServer(&sdk.Implementation{Name: "tunneld", Version: "0"}, &sdk.ServerOptions{
		Instructions: "Each tool names an origin by index n, as the origins tool lists them. " +
			"exec runs one command privately and returns its exit code; a session keeps a shell between calls.",
	})
	sdk.AddTool(srv, &sdk.Tool{Name: "origins", Description: "List the run's origins: index, origin, kind."}, s.listOrigins)
	sdk.AddTool(srv, &sdk.Tool{Name: "exec", Description: "Run argv once on origin n over pipes, no terminal; stdout, stderr and the exit code come back."}, s.exec)
	sdk.AddTool(srv, &sdk.Tool{Name: "put_file", Description: "Write bytes to an absolute path on origin n."}, s.putFile)
	sdk.AddTool(srv, &sdk.Tool{Name: "get_file", Description: "Read an absolute path on origin n."}, s.getFile)
	sdk.AddTool(srv, &sdk.Tool{Name: "session_open", Description: "Start a private process on origin n that outlives this call; argv defaults to the origin's own program."}, s.sessions.open)
	sdk.AddTool(srv, &sdk.Tool{Name: "session_write", Description: "Send stdin to a session."}, s.sessions.write)
	sdk.AddTool(srv, &sdk.Tool{Name: "session_read", Description: "Read what a session has printed, waiting up to timeout_ms for something."}, s.sessions.read)
	sdk.AddTool(srv, &sdk.Tool{Name: "session_close", Description: "End a session and its process."}, s.sessions.close)
	return srv, s.sessions
}

// origin answers origin n, or the tool error for an index off the list or
// an origin that cannot run a program.
func (s *server) origin(n int) (Origin, error) {
	if n < 0 || n >= len(s.origins) {
		return Origin{}, fmt.Errorf("origin %d: there are %d origins, 0 to %d", n, len(s.origins), len(s.origins)-1)
	}
	o := s.origins[n]
	if o.Spawner == nil {
		return Origin{}, fmt.Errorf("origin %d cannot run a program: it is a%s %s origin", n, article(o.Kind), o.Kind)
	}
	return o, nil
}

// article is the "n" in "an http origin".
func article(k Kind) string {
	if k == KindHTTP {
		return "n"
	}
	return ""
}
```

In this task, `sessions`, `newSessions`, `putFile` and `getFile` do not exist yet. Create them as minimal stubs so the package builds, in the files that will own them:

`v0exp1/internal/mcp/sessions.go` (stub, replaced in Task 7):

```go
package mcp

import (
	"context"
	"errors"
	"log/slog"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// sessions is the run's session table. Filled in by the sessions change.
type sessions struct{}

func newSessions([]Origin, *slog.Logger) *sessions { return &sessions{} }

func (*sessions) Close() error { return nil }

var errNoSessions = errors.New("sessions are not available yet")

type sessionOpenIn struct {
	N    int      `json:"n" jsonschema:"origin index, as origins lists"`
	Argv []string `json:"argv,omitempty" jsonschema:"the program and its arguments; omitted runs the origin's own"`
}
type sessionIDIn struct {
	ID string `json:"id" jsonschema:"the session id session_open returned"`
}
type sessionWriteIn struct {
	ID    string `json:"id" jsonschema:"the session id session_open returned"`
	Stdin string `json:"stdin" jsonschema:"bytes to send to the process's stdin"`
}
type sessionReadIn struct {
	ID        string `json:"id" jsonschema:"the session id session_open returned"`
	TimeoutMs int    `json:"timeout_ms,omitempty" jsonschema:"how long to wait for output when there is none; default 1000"`
}
type sessionOpenOut struct {
	ID string `json:"id"`
}
type sessionReadOut struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	Exited   bool   `json:"exited"`
	ExitCode int    `json:"exit_code,omitempty"`
}

func (*sessions) open(context.Context, *sdk.CallToolRequest, sessionOpenIn) (*sdk.CallToolResult, sessionOpenOut, error) {
	return nil, sessionOpenOut{}, errNoSessions
}
func (*sessions) write(context.Context, *sdk.CallToolRequest, sessionWriteIn) (*sdk.CallToolResult, any, error) {
	return nil, nil, errNoSessions
}
func (*sessions) read(context.Context, *sdk.CallToolRequest, sessionReadIn) (*sdk.CallToolResult, sessionReadOut, error) {
	return nil, sessionReadOut{}, errNoSessions
}
func (*sessions) close(context.Context, *sdk.CallToolRequest, sessionIDIn) (*sdk.CallToolResult, any, error) {
	return nil, nil, errNoSessions
}
```

`v0exp1/internal/mcp/files.go` (stub, replaced in Task 6):

```go
package mcp

import (
	"context"
	"errors"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const maxFile = 16 << 20 // 16 MiB per put_file or get_file

var errNoFiles = errors.New("files are not available yet")

type putFileIn struct {
	N             int    `json:"n" jsonschema:"origin index, as origins lists"`
	Path          string `json:"path" jsonschema:"absolute path on the origin"`
	ContentBase64 string `json:"content_base64" jsonschema:"the bytes, base64"`
	Mode          string `json:"mode,omitempty" jsonschema:"octal file mode, default 0644"`
}
type putFileOut struct {
	Bytes int `json:"bytes"`
}
type getFileIn struct {
	N    int    `json:"n" jsonschema:"origin index, as origins lists"`
	Path string `json:"path" jsonschema:"absolute path on the origin"`
}
type getFileOut struct {
	ContentBase64 string `json:"content_base64"`
	Bytes         int    `json:"bytes"`
	Mode          string `json:"mode"`
}

func (*server) putFile(context.Context, *sdk.CallToolRequest, putFileIn) (*sdk.CallToolResult, putFileOut, error) {
	return nil, putFileOut{}, errNoFiles
}
func (*server) getFile(context.Context, *sdk.CallToolRequest, getFileIn) (*sdk.CallToolResult, getFileOut, error) {
	return nil, getFileOut{}, errNoFiles
}
```

- [ ] **Step 4: Implement `tools.go`**

```go
package mcp

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxOutput        = 1 << 20 // 1 MiB each of stdout and stderr
	defaultTimeoutMs = 60_000
	maxTimeoutMs     = 600_000
)

type originRow struct {
	N      int    `json:"n"`
	Origin string `json:"origin"`
	Kind   Kind   `json:"kind"`
}

// listOrigins is the origins tool: every origin of the run, index first,
// whether or not the other tools can do anything with it.
func (s *server) listOrigins(context.Context, *sdk.CallToolRequest, any) (*sdk.CallToolResult, []originRow, error) {
	rows := make([]originRow, len(s.origins))
	for i, o := range s.origins {
		rows[i] = originRow{N: i, Origin: o.Name, Kind: o.Kind}
	}
	return nil, rows, nil
}

type execIn struct {
	N         int      `json:"n" jsonschema:"origin index, as origins lists"`
	Argv      []string `json:"argv" jsonschema:"the program and its arguments; on a shell origin, [\"-c\", \"...\"]"`
	Stdin     string   `json:"stdin,omitempty" jsonschema:"bytes for the program's stdin"`
	TimeoutMs int      `json:"timeout_ms,omitempty" jsonschema:"kill the program after this long; default 60000, at most 600000"`
}

type execOut struct {
	Stdout    string `json:"stdout"`
	Stderr    string `json:"stderr"`
	ExitCode  int    `json:"exit_code" jsonschema:"the program's exit code; -1 when it was killed, at the timeout or by a signal"`
	Truncated bool   `json:"truncated" jsonschema:"true when stdout or stderr passed 1 MiB and the rest was dropped"`
}

// exec is the exec tool: argv once, privately, on origin n.
func (s *server) exec(ctx context.Context, _ *sdk.CallToolRequest, in execIn) (*sdk.CallToolResult, execOut, error) {
	o, err := s.origin(in.N)
	if err != nil {
		return nil, execOut{}, err
	}
	timeout := time.Duration(clampTimeout(in.TimeoutMs)) * time.Millisecond
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	// A nil reader, not a reader of nothing: the spawner closes the
	// program's stdin at once for nil, and a shell given an empty pipe
	// behaves the same, so this is only about not allocating one.
	var stdin io.Reader
	if in.Stdin != "" {
		stdin = strings.NewReader(in.Stdin)
	}
	out, errb := newCapped(maxOutput), newCapped(maxOutput)
	start := time.Now()
	exit, err := o.Spawner.Spawn(ctx, in.Argv, stdin, out, errb)
	s.log.Debug("mcp exec", "origin", in.N, "argv", in.Argv, "exit", exit, "took", time.Since(start), "error", err)
	if err != nil {
		return nil, execOut{}, err
	}
	return nil, execOut{
		Stdout:    out.text(),
		Stderr:    errb.text(),
		ExitCode:  exit,
		Truncated: out.truncated || errb.truncated,
	}, nil
}

// clampTimeout is timeout_ms as asked, made sane: zero or less is the
// default, past the maximum is the maximum — an agent that asks for an hour
// gets ten minutes rather than a refusal.
func clampTimeout(ms int) int {
	switch {
	case ms <= 0:
		return defaultTimeoutMs
	case ms > maxTimeoutMs:
		return maxTimeoutMs
	}
	return ms
}

// capped is a writer that keeps the first max bytes and drops the rest,
// remembering that it did. Safe to write from the process's own goroutines.
type capped struct {
	mu        sync.Mutex
	buf       bytes.Buffer
	max       int
	truncated bool
}

func newCapped(max int) *capped { return &capped{max: max} }

// Write accepts every byte, kept or dropped: a short count would make
// io.Copy stop with io.ErrShortWrite and the program's pipe close under it.
func (c *capped) Write(p []byte) (int, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n := len(p)
	if room := c.max - c.buf.Len(); len(p) > room {
		c.truncated = true
		p = p[:room]
	}
	c.buf.Write(p)
	return n, nil
}

// text is what was kept, made valid UTF-8: JSON cannot carry a stray byte,
// and a program's output is not always text.
func (c *capped) text() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return strings.ToValidUTF8(c.buf.String(), "�")
}
```

In `exec`, add `"io"` to the imports. The test row "output past the cap is dropped and marked" is what catches a cap writer that returns a short count.

Then: `go mod tidy` — the SDK becomes a direct requirement.

- [ ] **Step 5: Run the tests to see them pass**

Run: `go test ./v0exp1/internal/mcp -count=1 -v`
Expected: PASS for `TestListsEveryTool`, `TestHandlerAcceptsAForwardedHost`, `TestOrigins`, `TestExec`, `TestExecTimeout`.

- [ ] **Step 6: Full gate and commit**

Run: `make fmt-check test`
Expected: `ℹ fail 0`; `readme_test.go` still passes with the SDK now direct.

```sh
git add go.mod go.sum v0exp1/internal/mcp
git commit -m "feat(mcp): the server, origins and exec (#243)

One MCP server over a run's origins, streamable HTTP, stateless, with
localhost protection off because the tunnel forwards the public Host to
a loopback listener. exec spawns privately, caps each stream at 1 MiB
and returns the real exit code; files and sessions are stubs here.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: `put_file` and `get_file`

**Files:**
- Modify: `v0exp1/internal/mcp/files.go` (replace the stub bodies)
- Create: `v0exp1/internal/mcp/files_test.go`

**Interfaces:**
- Consumes: `server.origin`, `Origin.Kind` (Task 5).
- Produces: the two tools as the spec's table; a program origin reads and writes tunneld's own filesystem; a container origin is refused.

- [ ] **Step 1: Write the failing test**

```go
package mcp

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"
)

// TestFiles pins put_file and get_file on a program origin: bytes round
// trip, the mode is applied, and the refusals — relative path, directory,
// too big, a container — are tool errors with nothing written.
func TestFiles(t *testing.T) {
	dir := t.TempDir()
	program := []Origin{{Name: "exec:///bin/sh", Kind: KindProgram, Spawner: &fakeSpawner{}}}
	container := []Origin{{Name: "attach://dockerd/web", Kind: KindContainer}}
	content := base64.StdEncoding.EncodeToString([]byte("hello\x00world"))

	t.Run("round trip with a mode", func(t *testing.T) {
		cs := connect(t, program)
		path := filepath.Join(dir, "a.bin")
		var put putFileOut
		if msg := call(t, cs, "put_file", map[string]any{"n": 0, "path": path, "content_base64": content, "mode": "0600"}, &put); msg != "" {
			t.Fatal(msg)
		}
		if put.Bytes != 11 {
			t.Errorf("put_file bytes = %d, want 11", put.Bytes)
		}
		if st, err := os.Stat(path); err != nil || st.Mode().Perm() != 0o600 {
			t.Errorf("stat = %v, %v; want mode 0600", st, err)
		}
		var got getFileOut
		if msg := call(t, cs, "get_file", map[string]any{"n": 0, "path": path}, &got); msg != "" {
			t.Fatal(msg)
		}
		if got.ContentBase64 != content || got.Bytes != 11 || got.Mode != "0600" {
			t.Errorf("get_file = %+v", got)
		}
	})

	for name, tc := range map[string]struct {
		origins []Origin
		tool    string
		args    map[string]any
		wantMsg string
	}{
		"a relative path is refused": {program, "put_file", map[string]any{"n": 0, "path": "rel.txt", "content_base64": content}, `path "rel.txt" is not absolute`},
		"a directory is refused":     {program, "put_file", map[string]any{"n": 0, "path": dir, "content_base64": content}, "is a directory"},
		"a missing file is refused":  {program, "get_file", map[string]any{"n": 0, "path": filepath.Join(dir, "nope")}, "no such file"},
		"a container is refused":     {container, "get_file", map[string]any{"n": 0, "path": "/etc/hostname"}, "origin 0 cannot run a program: it is a container origin"},
		"bad base64 is refused":      {program, "put_file", map[string]any{"n": 0, "path": filepath.Join(dir, "b"), "content_base64": "!!"}, "content_base64"},
	} {
		t.Run(name, func(t *testing.T) {
			cs := connect(t, tc.origins)
			msg := call(t, cs, tc.tool, tc.args, nil)
			if msg == "" || !contains(msg, tc.wantMsg) {
				t.Errorf("%s = %q, want an error containing %q", tc.tool, msg, tc.wantMsg)
			}
		})
	}
	if _, err := os.Stat(filepath.Join(dir, "rel.txt")); err == nil {
		t.Error("a refused put_file wrote a file")
	}
}
```

Write `contains` as `strings.Contains` (import `strings`); it is named here only to keep the row short.

- [ ] **Step 2: Run the test to see it fail**

Run: `go test ./v0exp1/internal/mcp -run TestFiles -v`
Expected: FAIL — every row returns "files are not available yet".

- [ ] **Step 3: Implement**

Replace `files.go`'s stub bodies (keep the types and `maxFile`):

```go
import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// fileOrigin is origin n as a place with a filesystem: a program origin,
// whose files are tunneld's own, as the user it runs as. A container has a
// filesystem too, but not one this build can reach — that comes with the
// docker provider's Spawner.
func (s *server) fileOrigin(n int, path string) error {
	o, err := s.origin(n)
	if err != nil {
		return err
	}
	if o.Kind != KindProgram {
		return fmt.Errorf("origin %d: files on a %s origin are not supported yet", n, o.Kind)
	}
	if !filepath.IsAbs(path) {
		return fmt.Errorf("path %q is not absolute", path)
	}
	return nil
}

func (s *server) putFile(_ context.Context, _ *sdk.CallToolRequest, in putFileIn) (*sdk.CallToolResult, putFileOut, error) {
	if err := s.fileOrigin(in.N, in.Path); err != nil {
		return nil, putFileOut{}, err
	}
	data, err := base64.StdEncoding.DecodeString(in.ContentBase64)
	if err != nil {
		return nil, putFileOut{}, fmt.Errorf("content_base64: %w", err)
	}
	if len(data) > maxFile {
		return nil, putFileOut{}, fmt.Errorf("content is %d bytes; at most %d", len(data), maxFile)
	}
	mode := os.FileMode(0o644)
	if in.Mode != "" {
		m, err := strconv.ParseUint(in.Mode, 8, 32)
		if err != nil {
			return nil, putFileOut{}, fmt.Errorf("mode %q is not octal", in.Mode)
		}
		mode = os.FileMode(m)
	}
	if err := os.WriteFile(in.Path, data, mode); err != nil {
		return nil, putFileOut{}, err
	}
	// WriteFile only applies the mode to a new file; a file that was there
	// keeps its own, so the mode asked for is set either way.
	if err := os.Chmod(in.Path, mode); err != nil {
		return nil, putFileOut{}, err
	}
	s.log.Debug("mcp put_file", "origin", in.N, "path", in.Path, "bytes", len(data))
	return nil, putFileOut{Bytes: len(data)}, nil
}

func (s *server) getFile(_ context.Context, _ *sdk.CallToolRequest, in getFileIn) (*sdk.CallToolResult, getFileOut, error) {
	if err := s.fileOrigin(in.N, in.Path); err != nil {
		return nil, getFileOut{}, err
	}
	f, err := os.Open(in.Path)
	if err != nil {
		return nil, getFileOut{}, err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, getFileOut{}, err
	}
	if st.IsDir() {
		return nil, getFileOut{}, fmt.Errorf("%s is a directory", in.Path)
	}
	if st.Size() > maxFile {
		return nil, getFileOut{}, fmt.Errorf("%s is %d bytes; at most %d", in.Path, st.Size(), maxFile)
	}
	data, err := io.ReadAll(io.LimitReader(f, maxFile))
	if err != nil {
		return nil, getFileOut{}, err
	}
	s.log.Debug("mcp get_file", "origin", in.N, "path", in.Path, "bytes", len(data))
	return nil, getFileOut{
		ContentBase64: base64.StdEncoding.EncodeToString(data),
		Bytes:         len(data),
		Mode:          fmt.Sprintf("%04o", st.Mode().Perm()),
	}, nil
}
```

`os.WriteFile` on a directory fails with "is a directory" on Unix; the row relies on that text. On Windows the error text differs; guard the "a directory is refused" row with `if runtime.GOOS == "windows" { t.Skip() }` inside its subtest.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./v0exp1/internal/mcp -count=1 -v -run TestFiles`
Expected: PASS.

- [ ] **Step 5: Full gate and commit**

Run: `make fmt-check test`
Expected: `ℹ fail 0`.

```sh
git add v0exp1/internal/mcp/files.go v0exp1/internal/mcp/files_test.go
git commit -m "feat(mcp): put_file and get_file on a program origin (#243)

Absolute paths, 16 MiB, the mode applied whether the file was there or
not. A container origin is refused until the docker provider can reach
its filesystem.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: Sessions

**Files:**
- Modify: `v0exp1/internal/mcp/sessions.go` (replace the stub)
- Create: `v0exp1/internal/mcp/sessions_test.go`
- Modify: `v0exp1/internal/mcp/mcp_test.go` (`TestListsEveryTool` needs no change; `fakeSpawner` gains nothing)

**Interfaces:**
- Consumes: `Origin`, `server.origin` semantics (the table owns a copy of the origins and the same index rule), `capped` (Task 5).
- Produces: `sessions` with `open`, `write`, `read`, `close` tool handlers and `Close() error`; constants `maxSessions = 16`, `sessionIdle = 10 * time.Minute`, `defaultReadMs = 1000`.

- [ ] **Step 1: Write the failing test**

```go
package mcp

import (
	"context"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

// echoSpawner is a process that echoes each line of stdin back with a
// prefix until stdin closes, then exits 5 — enough of a shell to pin that
// a session keeps one process across calls.
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
// ends it with its exit.
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
// exited, with its code, and the session is then forgotten.
func TestSessionReportsTheExit(t *testing.T) {
	cs := connect(t, []Origin{{Kind: KindProgram, Spawner: &fakeSpawner{out: "bye\n", exit: 4}}})
	var opened sessionOpenOut
	if msg := call(t, cs, "session_open", map[string]any{"n": 0, "argv": []string{"true"}}, &opened); msg != "" {
		t.Fatal(msg)
	}
	var got sessionReadOut
	deadline := time.Now().Add(5 * time.Second)
	for !got.Exited && time.Now().Before(deadline) {
		if msg := call(t, cs, "session_read", map[string]any{"id": opened.ID, "timeout_ms": 200}, &got); msg != "" {
			t.Fatal(msg)
		}
	}
	if !got.Exited || got.ExitCode != 4 {
		t.Errorf("session_read = %+v, want exited with 4", got)
	}
	if msg := call(t, cs, "session_read", map[string]any{"id": opened.ID}, nil); !strings.Contains(msg, "no session") {
		t.Errorf("a read after the exit was reported = %q, want no session", msg)
	}
}

// TestSessionLimits pins the table's edges: the seventeenth open is refused
// and an idle session is reaped; Close ends them all.
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
}
```

- [ ] **Step 2: Run the tests to see them fail**

Run: `go test ./v0exp1/internal/mcp -run TestSession -v`
Expected: FAIL — "sessions are not available yet" / undefined `tbl.mu`.

- [ ] **Step 3: Implement `sessions.go`**

Replace the whole stub, keeping the `session*In`/`Out` types:

```go
package mcp

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	maxSessions   = 16
	sessionIdle   = 10 * time.Minute
	defaultReadMs = 1000
	readPoll      = 25 * time.Millisecond
)

// sessions is the run's session table: private processes that outlive the
// call that started them, so an agent keeps a cwd and an environment
// between commands the way a person does in the shared terminal.
type sessions struct {
	origins []Origin
	log     *slog.Logger

	mu    sync.Mutex
	table map[string]*session
	// ctx is the table's life: every session's process ends with it.
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{} // the reaper has stopped
}

// session is one process: its stdin, what it has printed since the last
// read, when it was last used, and how it ended.
type session struct {
	cancel context.CancelFunc
	stdin  io.WriteCloser
	out    *capped
	errb   *capped
	exited chan struct{}
	exit   int
	last   time.Time
}

func newSessions(origins []Origin, log *slog.Logger) *sessions {
	ctx, cancel := context.WithCancel(context.Background())
	t := &sessions{origins: origins, log: log, table: map[string]*session{}, ctx: ctx, cancel: cancel, done: make(chan struct{})}
	go t.reaper()
	return t
}

// Close ends every session and the reaper.
func (t *sessions) Close() error {
	t.cancel()
	<-t.done
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, s := range t.table {
		s.cancel()
		delete(t.table, id)
	}
	return nil
}

// reaper ends sessions nothing has touched for sessionIdle, once a minute.
func (t *sessions) reaper() {
	defer close(t.done)
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-t.ctx.Done():
			return
		case <-tick.C:
			t.reap()
		}
	}
}

func (t *sessions) reap() {
	t.mu.Lock()
	defer t.mu.Unlock()
	for id, s := range t.table {
		if time.Since(s.last) > sessionIdle {
			t.log.Debug("mcp session reaped", "id", id)
			s.cancel()
			delete(t.table, id)
		}
	}
}

func (t *sessions) count() int {
	t.mu.Lock()
	defer t.mu.Unlock()
	return len(t.table)
}

// get answers the live session with that id, touching it.
func (t *sessions) get(id string) (*session, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.table[id]
	if !ok {
		return nil, fmt.Errorf("no session %q", id)
	}
	s.last = time.Now()
	return s, nil
}

func (t *sessions) open(_ context.Context, _ *sdk.CallToolRequest, in sessionOpenIn) (*sdk.CallToolResult, sessionOpenOut, error) {
	if in.N < 0 || in.N >= len(t.origins) {
		return nil, sessionOpenOut{}, fmt.Errorf("origin %d: there are %d origins, 0 to %d", in.N, len(t.origins), len(t.origins)-1)
	}
	o := t.origins[in.N]
	if o.Spawner == nil {
		return nil, sessionOpenOut{}, fmt.Errorf("origin %d cannot run a program: it is a%s %s origin", in.N, article(o.Kind), o.Kind)
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.table) >= maxSessions {
		return nil, sessionOpenOut{}, fmt.Errorf("%d sessions are open, the most this run allows; close one", maxSessions)
	}
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return nil, sessionOpenOut{}, err
	}
	id := hex.EncodeToString(raw[:])
	ctx, cancel := context.WithCancel(t.ctx)
	pr, pw := io.Pipe()
	s := &session{cancel: cancel, stdin: pw, out: newCapped(maxOutput), errb: newCapped(maxOutput), exited: make(chan struct{}), last: time.Now()}
	t.table[id] = s
	go func() {
		exit, err := o.Spawner.Spawn(ctx, in.Argv, pr, s.out, s.errb)
		_ = pr.Close()
		if err != nil {
			_, _ = io.WriteString(s.errb, err.Error()+"\n")
			exit = -1
		}
		s.exit = exit
		close(s.exited)
		t.log.Debug("mcp session ended", "id", id, "origin", in.N, "exit", exit, "error", err)
	}()
	t.log.Debug("mcp session opened", "id", id, "origin", in.N, "argv", in.Argv)
	return nil, sessionOpenOut{ID: id}, nil
}

func (t *sessions) write(_ context.Context, _ *sdk.CallToolRequest, in sessionWriteIn) (*sdk.CallToolResult, any, error) {
	s, err := t.get(in.ID)
	if err != nil {
		return nil, nil, err
	}
	select {
	case <-s.exited:
		return nil, nil, fmt.Errorf("session %q has exited with %d; read it", in.ID, s.exit)
	default:
	}
	// A write blocks until the process reads it; a process that has stopped
	// reading would hold the call forever, so it is given a moment.
	done := make(chan error, 1)
	go func() { _, err := io.WriteString(s.stdin, in.Stdin); done <- err }()
	select {
	case err := <-done:
		return nil, nil, err
	case <-time.After(5 * time.Second):
		return nil, nil, fmt.Errorf("session %q is not reading its stdin", in.ID)
	case <-s.exited:
		return nil, nil, fmt.Errorf("session %q exited with %d while being written to", in.ID, s.exit)
	}
}

func (t *sessions) read(ctx context.Context, _ *sdk.CallToolRequest, in sessionReadIn) (*sdk.CallToolResult, sessionReadOut, error) {
	s, err := t.get(in.ID)
	if err != nil {
		return nil, sessionReadOut{}, err
	}
	wait := in.TimeoutMs
	if wait <= 0 {
		wait = defaultReadMs
	}
	deadline := time.Now().Add(time.Duration(wait) * time.Millisecond)
	for {
		out, errText := s.out.take(), s.errb.take()
		exited := false
		select {
		case <-s.exited:
			exited = true
		default:
		}
		if out != "" || errText != "" || exited || time.Now().After(deadline) || ctx.Err() != nil {
			res := sessionReadOut{Stdout: out, Stderr: errText, Exited: exited}
			if exited {
				res.ExitCode = s.exit
				t.mu.Lock()
				delete(t.table, in.ID)
				t.mu.Unlock()
			}
			return nil, res, nil
		}
		time.Sleep(readPoll)
	}
}

func (t *sessions) close(_ context.Context, _ *sdk.CallToolRequest, in sessionIDIn) (*sdk.CallToolResult, any, error) {
	t.mu.Lock()
	s, ok := t.table[in.ID]
	delete(t.table, in.ID)
	t.mu.Unlock()
	if !ok {
		return nil, nil, fmt.Errorf("no session %q", in.ID)
	}
	_ = s.stdin.Close()
	s.cancel()
	<-s.exited
	t.log.Debug("mcp session closed", "id", in.ID, "exit", s.exit)
	return nil, nil, nil
}
```

Add to `capped` in `tools.go`:

```go
// take is what arrived since the last take, as text, and the buffer is
// emptied; the cap applies per take, so a session that is read keeps
// printing.
func (c *capped) take() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := strings.ToValidUTF8(c.buf.String(), "�")
	c.buf.Reset()
	c.truncated = false
	return s
}
```

Note: `TestSessionReportsTheExit`'s "forgotten after the exit was reported" needs `read` to delete the row once it has said `exited`; the code above does. A session whose process exited but was never read stays until the reaper takes it.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./v0exp1/internal/mcp -count=1 -race -v`
Expected: PASS, race-clean.

- [ ] **Step 5: Full gate and commit**

Run: `make fmt-check test`
Expected: `ℹ fail 0`.

```sh
git add v0exp1/internal/mcp/sessions.go v0exp1/internal/mcp/sessions_test.go v0exp1/internal/mcp/tools.go
git commit -m "feat(mcp): sessions keep one private process between calls (#243)

session_open starts it, write feeds it, read takes what arrived since
the last read and reports the exit once, close ends it. Sixteen per run,
reaped after ten idle minutes, all ended with the handler.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: The experiment facade

**Files:**
- Modify: `v0exp1/v0exp1.go`
- Modify: `v0exp1/v0exp1_test.go`

**Interfaces:**
- Consumes: `mcp.Handler`, `mcp.Origin`, `mcp.Kind`, `mcp.Spawner` (Task 5).
- Produces:
  ```go
  type Experiments interface { Builtin() Builtin; Mcp() Mcp }
  type Mcp interface { Handler(origins []McpOrigin, log *slog.Logger) (http.Handler, io.Closer) }
  type McpOrigin = mcp.Origin
  type McpKind = mcp.Kind
  type McpSpawner = mcp.Spawner
  const ( McpProgram = mcp.KindProgram; McpContainer = mcp.KindContainer; McpHTTP = mcp.KindHTTP )
  ```
  Prefixed `Mcp*` because `v0exp1` is one package for every experiment and `Origin` on its own would claim the word.

- [ ] **Step 1: Write the failing test**

Append to `v0exp1/v0exp1_test.go`:

```go
// TestMcp holds Experimental().Mcp() to the package it fronts: a handler
// that answers, and a closer that can be closed twice.
func TestMcp(t *testing.T) {
	m := Experimental().Mcp()
	if m == nil {
		t.Skip("the MCP server is turned off")
	}
	h, closer := m.Handler(nil, slog.New(slog.DiscardHandler))
	if h == nil || closer == nil {
		t.Fatalf("Handler = %v, %v; want both", h, closer)
	}
	if err := closer.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	var _ McpOrigin = McpOrigin{Kind: McpProgram}
}
```

Add `"log/slog"` to the imports.

- [ ] **Step 2: Run the test to see it fail**

Run: `go test ./v0exp1 -run TestMcp -v`
Expected: FAIL to compile — `Mcp undefined`.

- [ ] **Step 3: Implement**

In `v0exp1/v0exp1.go`, import `"io"`, `"log/slog"`, `"net/http"` and `"github.com/tunnel-pizza/tunneld/v0exp1/internal/mcp"`; extend the package comment's example list with `v0exp1.Experimental().Mcp().Handler(origins, log)`; then:

```go
type Experiments interface {
	// Builtin is the shell built into tunneld, for a machine with none; nil
	// when it is turned off.
	Builtin() Builtin
	// Mcp is the MCP server tunneld serves to agents on its control path;
	// nil when it is turned off.
	Mcp() Mcp
}

// Mcp serves one run's origins to agents over streamable HTTP: tools that
// run a command, move a file or hold a shell on any origin that can spawn a
// process, by the index the routing parameter uses.
type Mcp interface {
	// Handler answers the MCP endpoint for these origins, index n being
	// origin n, logging each call on log. Closing stops every session and
	// process it started.
	Handler(origins []McpOrigin, log *slog.Logger) (http.Handler, io.Closer)
}

// McpOrigin is what the server is told about one origin: how it is shown,
// what it is, and what can spawn a process on it, nil for nothing.
type McpOrigin = mcp.Origin

// McpKind is what an origin is: McpProgram, McpContainer or McpHTTP.
type McpKind = mcp.Kind

const (
	McpProgram   = mcp.KindProgram
	McpContainer = mcp.KindContainer
	McpHTTP      = mcp.KindHTTP
)

// McpSpawner starts one private process on an origin; attach.Spawner has
// the same shape, so a bound origin's satisfies it.
type McpSpawner = mcp.Spawner

// Mcp is the MCP server.
func (ExperimentsImpl) Mcp() Mcp { return McpImpl{} }

// McpImpl is Mcp, over v0exp1/internal/mcp.
type McpImpl struct{}

// Handler is mcp.Handler.
func (McpImpl) Handler(origins []McpOrigin, log *slog.Logger) (http.Handler, io.Closer) {
	return mcp.Handler(origins, log)
}
```

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./v0exp1 -count=1 -v`
Expected: PASS.

- [ ] **Step 5: Full gate and commit**

Run: `make fmt-check test`
Expected: `ℹ fail 0`.

```sh
git add v0exp1/v0exp1.go v0exp1/v0exp1_test.go
git commit -m "feat(v0exp1): Mcp(), the way in to the agent server (#243)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: The builder mounts it

**Files:**
- Modify: `v1alpha1/builder.go` (after `defer bound.Close()` at ~535; the `router.Route` options at ~640-655)
- Modify: `v1alpha1/builder_test.go` (`TestRun` table; `fakeBinder.spawners` from Task 3)

**Interfaces:**
- Consumes: `bound.Spawners()` (Task 3), `v0exp1.Experimental().Mcp()` and the `Mcp*` types (Task 8), `router.WithHandler`/`Mounted` (Task 2).
- Produces: `/_tunneld/mcp` mounted when the experiment is on; the handler closed when the run ends.

- [ ] **Step 1: Write the failing test**

In `TestRun` in `builder_test.go`, beside the case "a binder failure is returned before the engine is asked for anything" (~1354), add one in the same shape — `newRunHarness(t, live(public), urls...)` builds the run, `h.run(t, ctx)` runs it:

```go
	t.Run("the MCP server is mounted on the control path", func(t *testing.T) {
		h := newRunHarness(t, live(public), ":3000", "exec:///bin/sh")
		h.binder.spawners = []attach.Spawner{nil, fakeSpawner{}}

		if err := h.run(t, t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := h.router.configured.Mounted(); !slices.Contains(got, router.ControlPath+"mcp") {
			t.Errorf("mounted = %v, want %s", got, router.ControlPath+"mcp")
		}
	})
```

If `exec:///bin/sh` is refused by the harness's origin settling (the parser resolves it against this machine; `/bin/sh` exists on every CI runner but Windows), the second origin may be `":4000"` instead — the case is about the mount, not the kinds. Add beside `fakeBinder`:

```go
// fakeSpawner is a Spawner that runs nothing: the run only has to hand it on.
type fakeSpawner struct{}

func (fakeSpawner) Spawn(context.Context, []string, io.Reader, io.Writer, io.Writer) (int, error) {
	return 0, nil
}
```

- [ ] **Step 2: Run the test to see it fail**

Run: `go test ./v1alpha1 -run 'TestRun/the_MCP' -v`
Expected: FAIL — mounted is empty.

- [ ] **Step 3: Implement**

In `builder.go`, after `defer bound.Close()`:

```go
	// The agent server, when the experiment is on: one over every shown
	// origin, each with its spawner where the bound origin has one, so a
	// tool that names origin n reaches what ?n reaches. Closed with the
	// bound origins, since what it holds are their processes.
	var agentOpts []router.Option
	if m := v0exp1.Experimental().Mcp(); m != nil {
		spawners := bound.Spawners()
		list := make([]v0exp1.McpOrigin, 0, origins.Len())
		for i, u := range origins.URLs() {
			o := v0exp1.McpOrigin{Name: u.String(), Kind: v0exp1.McpHTTP}
			switch u.Scheme {
			case v1.ExecScheme:
				o.Kind = v0exp1.McpProgram
			case v1.AttachScheme:
				o.Kind = v0exp1.McpContainer
			}
			if i < len(spawners) && spawners[i] != nil {
				o.Spawner = spawners[i]
			}
			list = append(list, o)
		}
		h, closer := m.Handler(list, log)
		defer closer.Close()
		agentOpts = append(agentOpts, router.WithHandler(router.ControlPath+"mcp", h))
	}
```

Then in the `b.router.Route(ctx, ...)` call, add `agentOpts...` — since Go cannot splice a slice among fixed arguments, build the option list first:

```go
	opts := []router.Option{
		router.WithOrigins(dialable),
		router.WithWebSockets(ws),
		router.WithWrap(b.display.Panel(b.multiview, origins, log)),
		router.WithCache(spec),
		router.WithAuth(b.auth),
		router.WithAllowOrigin(router.ProviderOrigin(cmp.Or(b.provider, v1.DefaultProvider))),
		router.WithLog(log),
	}
	local, err := b.router.Route(ctx, append(opts, agentOpts...)...)
```

Keep the existing comments on the cache and the `TODO(#209)` where they were.

A spawner from `bound.Spawners()` is an `attach.Spawner`; it satisfies `v0exp1.McpSpawner` by shape, so no adapter is needed. `nil` must stay nil in `o.Spawner`: assigning a nil `attach.Spawner` interface to the `McpSpawner` field makes a non-nil interface holding nil only if done through a typed value, which the `if` above avoids.

- [ ] **Step 4: Run the tests to see them pass**

Run: `go test ./v1alpha1 -count=1 -run TestRun -v`
Expected: PASS, the new case included; the spec-cache tracking cases unchanged.

- [ ] **Step 5: Full gate and commit**

Run: `make fmt-check test`
Expected: `ℹ fail 0`.

```sh
git add v1alpha1/builder.go v1alpha1/builder_test.go
git commit -m "feat: serve the agent MCP server at /_tunneld/mcp (#243)

Mounted when the experiment is on, over every shown origin with its
spawner where the bound origin has one, behind the tunnel secret the
control path already demands. Closed with the bound origins.

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 10: Documentation

**Files:**
- Modify: `docs/reference.md` (after the control-path table at ~187 and its `curl` block at ~201)
- Modify: `CONTRIBUTING.md` (file map rows ~22-28; "Container origins" ~425-435; "Adding a collaborator" is not changed — this is not a collaborator)
- Modify: `docs/embedding.md` only if it names `router.WithHandler` or `Experimental()` (`grep -n "WithHandler\|Experimental" docs/embedding.md`; at the time of writing it names neither, so no change)

- [ ] **Step 1: The reference**

In `docs/reference.md`, add a row to the control-path table:

```markdown
| `POST /_tunneld/mcp` | token | an MCP server over the run's origins, for agents — see [Agents](#agents) |
```

Then, after the `curl` block that ends the control-path section and before `## Programs`, add:

```markdown
### Agents

A run serves one [MCP](https://modelcontextprotocol.io) server to agents, over
streamable HTTP at `/_tunneld/mcp`. It is for whoever holds the tunnel's
secret, like everything else under `/_tunneld/`: a password on the tunnel
does not open it, and the secret opens it whether or not there is a
password. Whoever has the secret could already respec the run, so the server
grants nothing new; it only makes a program origin usable without a
terminal.

```sh
claude mcp add --transport http tunneld https://<host>/_tunneld/mcp \
  --header "Authorization: token <secret>"
```

The tools name an origin by its index `n`, the number `?n` and the stderr
map use. `origins` lists them. The other tools work on an origin that can
start a process — today a program, `exec://`; a container is listed and
refused until its provider can — and spawn a private one, over pipes with no
terminal, beside the shared terminal a person may be watching.

| Tool | Does |
| ---- | ---- |
| `origins()` | `[{n, origin, kind}]`, kind one of `program`, `container`, `http` |
| `exec(n, argv, stdin?, timeout_ms?)` | runs `argv` once; `{stdout, stderr, exit_code, truncated}` |
| `put_file(n, path, content_base64, mode?)` / `get_file(n, path)` | bytes to and from an absolute path, 16 MiB at most |
| `session_open(n, argv?)` → `{id}`; `session_write(id, stdin)`; `session_read(id, timeout_ms?)`; `session_close(id)` | one process that outlives its calls; `argv` defaults to the origin's own program |

`exec` on a shell origin takes `argv: ["-c", "…"]`. Its `timeout_ms` is
60 000 by default and 600 000 at most; at the timeout the process group is
killed and `exit_code` is `-1`. Each of `stdout` and `stderr` keeps its first
1 MiB and `truncated` says when the rest was dropped. The edge answers a
request nothing has been written to for about 100 seconds with a `524`, so a
command expected to run longer belongs in a session, read as it goes. A
session is reaped after ten idle minutes; a run holds sixteen at most. Paths
and processes are tunneld's own user: there is no sandbox, the secret is the
whole of the guard.
```

- [ ] **Step 2: CONTRIBUTING**

File map, after the `v0exp1/internal/shell/builtin/` row:

```markdown
| The MCP server served to agents on the control path: tools over a run's origins, sessions | [`v0exp1/internal/mcp/`](./v0exp1/internal/mcp) |
```

In "Container origins", after the paragraph beginning "`docker.TargetImpl` is that provider, and `shell.TargetImpl` is the second", add:

```markdown
A provider may also implement `attach.Spawner` — one private process over
pipes, with its exit code — which is what the agent server's tools run on.
`shell.TargetImpl` does; `docker.TargetImpl` does not yet, so a container is
listed to an agent and refused. `Bind` keeps each spawner at its origin's
index on `Bound.Spawners`, nil where there is none.
```

In the `router/` file map row, extend the topic: `…and the page for an origin nothing answers on; \`WithHandler\` mounts on the control path (\`Router\`)`.

- [ ] **Step 3: Check the docs build nothing breaks**

Run: `make fmt-check test`
Expected: `ℹ fail 0` (`readme_test.go` reads README, not the reference, but the gate is the rule).

- [ ] **Step 4: Commit**

```sh
git add docs/reference.md CONTRIBUTING.md
git commit -m "docs: the agent MCP server, its tools and its guard (#243)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 11: Live verification and the mutation pass

**Files:** none changed unless the run finds something.

- [ ] **Step 1: Build and run a tunnel with a shell origin**

```sh
go build -o /tmp/tunneld-mcp . && /tmp/tunneld-mcp --log-level debug exec:///bin/sh
```

Note the hostname on stdout. Get the secret without printing it: `GET /_tunneld/.env` needs the token, which is in the cache file; read the cache file's path from the debug log and extract the secret into a `chmod 600` file in the scratchpad, never onto the terminal.

- [ ] **Step 2: Call it with a real harness**

```sh
claude mcp add --transport http tunneld-live https://<host>/_tunneld/mcp \
  --header "Authorization: token $(cat <scratch>/token)"
```

In a Claude Code session: `origins`, then `exec` with `argv: ["-c", "echo hi; echo err >&2; exit 3"]`. Expect `stdout "hi\n"`, `stderr "err\n"`, `exit_code 3`. Then `session_open`, `session_write "cd /tmp\n"`, `session_write "pwd\n"`, `session_read` → `/tmp`. Then `put_file` a small file and `get_file` it back. Record exact results in the PR body.

Remove the harness entry afterwards: `claude mcp remove tunneld-live`. Delete the token file.

- [ ] **Step 3: Mutation pass over tools.go**

Follow the memory `mutation-harness-on-macos.md` (no `timeout` here; `go test -timeout`; never pipe the harness). Mutate `clampTimeout`'s comparisons, `capped.Write`'s `len(p) > room`, and `origin`'s bounds check. Every mutant must be killed by `go test ./v0exp1/internal/mcp`. A survivor is a missing test row: add it to the owning `_test.go` and commit it under `test(mcp): …`.

- [ ] **Step 4: The pre-push gate**

```sh
make fmt-check test race
go test -short ./e2e -count=1
GOOS=linux go vet ./... && GOOS=windows go vet ./...
```

Expected: all clean, e2e mints nothing. Do not push: say it is ready and wait to be told.
