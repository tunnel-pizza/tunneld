# An MCP surface for agents — design

Issue: [#243](https://github.com/tunnel-pizza/tunneld/issues/243). Siblings:
#244 (one-shot `POST /exec`), #245 (isolated sessions, closed by this),
#246 (document the `/attach` protocol), #208 (agent handoffs).

## Intent

An agent that reaches a tunneld-served program today drives the xterm page
through a browser: it types into a canvas, reads pixels back, and appends
`echo DONE` sentinels to learn when a command finished. Everything it needs
is already on the wire — `/attach` speaks `v4.channel.k8s.io`, with separate
stdout and stderr and an exit status on the error stream — but there is no
agent entry point, and the one process behind `/attach` is shared with every
human viewer.

This design gives a run one MCP server, served by tunneld itself over
streamable HTTP, that knows every origin of the run and lets the holder of
the tunnel's secret run commands, move files and hold a private shell on the
origins that can spawn a process. Humans keep the page and the shared
`/attach` exactly as they are.

What Christian decided, verbatim where it matters:

- "keep a light touch and just do an Experiments interface, Mcp() that is
  aware of all origins" — no new origin scheme, no `+mcp` marker, no change
  to `servedSchemes` or the binder's dispatch.
- "lets have router.WithHandler(ControlPath, mcp.Handler())" — the server
  is mounted on the router's own mux under the control path, so the tunnel
  secret authorizes it with no new auth code.
- The stdio shim (`npx tunneld mcp …`) is deferred to its own issue: every
  current harness speaks streamable HTTP with an auth header.

## Shape

```
agent ──streamable HTTP──▶ https://<host>/_tunneld/mcp
        Authorization: token <base64 secret>
                                │
                      router.authorize (401 without the secret)
                                │
                      v0exp1/internal/mcp.Handler  ── tools ──┐
                                                             │
         origin 0: exec:///bin/sh      shell.TargetImpl.Spawn ◀┘
         origin 1: attach://dockerd/x  (no Spawner yet: tools refuse)
         origin 2: http://…:3000       (never spawns: tools refuse)
```

One server per run, not per origin. A tool names its origin by index `n`,
the same number the bare `?n` routing parameter and the stderr origin map
use. The handler is built once per run after `Bind`, alongside the bound
origins, and goes away with them.

### The experiment

`v0exp1.Experiments` grows a method, following `Builtin`:

```go
type Experiments interface {
	Builtin() Builtin
	// Mcp is the MCP server tunneld serves to agents; nil when turned off.
	Mcp() Mcp
}

// Mcp serves one run's origins to agents over streamable HTTP.
type Mcp interface {
	// Handler answers the MCP endpoint for these origins, index n being
	// origin n. Closing stops every session and process it started.
	Handler(origins []Origin) (http.Handler, io.Closer)
}

// Origin is what the server knows about one origin of the run.
type Origin struct {
	Name    string  // the origin as shown, e.g. exec:///bin/sh
	Kind    Kind    // program, container, http
	Spawner Spawner // nil when the origin cannot start a process
}

// Spawner starts one private process on an origin, over pipes, and
// reports how it ended. The process ends with ctx.
type Spawner interface {
	Spawn(ctx context.Context, argv []string, stdin io.Reader, stdout, stderr io.Writer) (exit int, err error)
}
```

`v0exp1` declares the types it consumes; `v1alpha1` adapts to them. The
dependency direction is the one `Builtin` already set (`v1alpha1` imports
`v0exp1`), so there is no cycle. The MCP Go SDK
(`github.com/modelcontextprotocol/go-sdk`, v1.8.0) is imported by
`v0exp1/internal/mcp` and nowhere else.

### The router

`router.WithHandler` today wraps the routing handler with the display's
panel. It is renamed `WithWrap`, which says what it does, and the name is
given to a mount:

```go
// WithWrap sets what wraps the routing handler — the display's Panel — or
// nothing, when nil.
func WithWrap(wrap func(http.Handler) http.Handler) Option

// WithHandler puts h on the router's mux at pattern, which must be under
// ControlPath: that prefix is the mux's and authorize's; every other path
// is an origin's. A pattern outside it is refused when the router routes.
func WithHandler(pattern string, h http.Handler) Option
```

The builder mounts the server with
`router.WithHandler(router.ControlPath+"mcp", h)`. `authorize` already
guards everything under `ControlPath` but ping, login and logout, so the
endpoint demands `Authorization: token <base64 secret>` before the SDK sees
a byte. Cache and auth keep their `Handlers(path)` maps because they are
swapped on a respec; the MCP handler is static for a run and a plain
`http.Handler` is enough.

### The provider primitive

`attach.Spawner` is declared beside `attach.Target`, and `attach.Bound`
grows `Spawners() []Spawner`, same length and order as the origins, nil
where an origin cannot spawn. `shell.TargetImpl` implements it now: a
process from `exec.CommandContext` with `pipeArgs`, `TERM=dumb`, its own
process group and the existing `pipeWait` reaping, stdin from the reader,
stdout and stderr to the writers, exit code from `ProcessState`. The docker
provider does not implement it in this change (see Out of scope), so a
container origin is listed and refused.

## Tools

All tools answer on `/_tunneld/mcp`. A tool that cannot do what it was
asked returns a tool result with `isError: true` and a one-line reason,
never a protocol error. A process that exits non-zero is not an error: the
code is in the result.

| Tool | Arguments | Result |
|---|---|---|
| `origins` | — | `[{n, origin, kind}]`, kind one of `program`, `container`, `http` |
| `exec` | `n`, `argv []string`, `stdin?`, `timeout_ms?` | `{stdout, stderr, exit_code, truncated}` |
| `put_file` | `n`, `path`, `content_base64`, `mode?` | `{bytes}` |
| `get_file` | `n`, `path` | `{content_base64, bytes, mode}` |
| `session_open` | `n`, `argv?` | `{id}` |
| `session_write` | `id`, `stdin` | `{}` |
| `session_read` | `id`, `timeout_ms?` | `{stdout, stderr, exited, exit_code?}` |
| `session_close` | `id` | `{}` |

- `exec` spawns a private process over pipes, no pty. With a shell origin,
  `argv` runs as given; `["-c", "…"]` is the ordinary call. The default
  `timeout_ms` is 60 000, the maximum 600 000; on timeout the process group
  is killed and the result carries what was read with `exit_code: -1`.
  Each of stdout and stderr is capped at 1 MiB; past that the rest is
  dropped and `truncated` is true.
- `put_file` and `get_file` take absolute paths. On a program origin with
  an empty authority they read and write tunneld's own filesystem as its
  user, 16 MiB at most. A container origin refuses until the docker
  provider can spawn (it will use the engine's copy endpoints then).
- A session is one process that outlives its calls: `argv` defaults to the
  origin's program, so cwd and environment persist across writes.
  `session_read` returns what has arrived, waiting up to `timeout_ms`
  (default 1 000) for something to arrive when nothing has. At most 16
  sessions per run; one idle for 10 minutes is reaped; all are killed when
  the handler closes.
- An `http` origin, and any origin with no `Spawner`, refuses `exec`,
  `put_file`, `get_file` and `session_open` with "origin n cannot run a
  program" naming its kind. There is no fetch tool.

## Errors and security

- **Who gets in.** The tunnel secret, and only the tunnel secret. Whoever
  holds it can already `PATCH /_tunneld/.env` and respec the run onto
  `exec:///bin/sh`, so the endpoint adds no privilege. A password-protected
  tunnel (#231) does not open it: the password is for visitors, the control
  path has always been the operator's. The reference says so.
- **No sandbox.** Paths and processes are the user tunneld runs as. The
  reference says so in the same breath as the auth.
- **Processes.** Own process group; killed on timeout, cancel, session close
  or handler close; `WaitDelay` of 2 s as the pipes mode has today. Nothing
  outlives a run.
- **Logging.** One debug line per tool call: tool, `n`, duration, exit.
  `argv` at debug. Never stdin, file bytes or output.
- **Browsers.** No CORS on the endpoint; the SDK's handler is same-origin
  by default. A browser-resident agent is not a target.

## Code layout

- `v0exp1/v0exp1.go` — `Mcp()` on `Experiments`; `Mcp`, `Origin`, `Kind`,
  `Spawner`; `McpImpl` forwarding to the internal package.
- `v0exp1/internal/mcp/mcp.go` — server construction, `Handler`;
  `tools.go` — `origins`, `exec`, `put_file`, `get_file`; `sessions.go` —
  the session table and its reaper.
- `v1alpha1/attach/attach.go` — `Spawner`; `Bound.Spawners()`.
- `v1alpha1/attach/shell/spawn.go` — `TargetImpl.Spawn`.
- `v1alpha1/router/router.go` — `WithWrap`, `WithHandler(pattern, h)`.
- `v1alpha1/builder.go` — after `Bind`, build the origin list from `bound`
  and mount when `Experimental().Mcp()` is non-nil; close with `bound`.

## Testing

One test file per source file, cases joining the existing tables:

- `v0exp1/internal/mcp/*_test.go` — the SDK's `NewInMemoryTransports`
  with a `ClientSession` calling each tool against a fake `Spawner`:
  exit codes, truncation, timeout, the refusals, session persistence and
  reaping.
- `v1alpha1/attach/shell/spawn_test.go` — real `/bin/sh` (real paths, not
  copied binaries): exit code, stdin reaches the process, process group
  dies on cancel.
- `v1alpha1/router/router_test.go` — a pattern outside `ControlPath` is
  refused; a mounted handler answers 401 without the token and 200 with.
- `v1alpha1/builder_test.go` — `Mcp()` nil mounts nothing; non-nil answers
  on `/_tunneld/mcp`.
- `v0exp1/v0exp1_test.go` — `Mcp()` is non-nil and forwards.
- Live, before the PR: a real run, `claude mcp add --transport http` with
  the token header, `origins` then `exec` on `exec:///bin/sh`. A mutation
  pass over `tools.go`. `go test -short ./e2e` mints nothing new.

## Documentation

- `README.md` Acknowledgements: a line for the MCP Go SDK (`readme_test.go`
  holds it to `go.mod`). "What you can share" is unchanged: nothing new is
  shared.
- `docs/reference.md`: an "Agents" subsection under the control path:
  endpoint, auth, the tool table, limits, and the two sentences on who gets
  in and what they get.
- `docs/embedding.md`: `WithWrap`, `WithHandler`, `Experimental().Mcp()`.
- `CONTRIBUTING.md`: "Container origins" notes a provider may implement
  `Spawner`; the "Adding a collaborator" checklist gains the row.

## Out of scope, each its own issue

- Docker `Spawner` via `ContainerExecCreate`, and file copy via the
  engine's copy endpoints.
- `tunneld mcp` — a stdio shim that reads the run's cache file for the URL
  and the secret, so an agent on the same machine needs no configuration.
- `POST /exec` (#244) and the `/attach` protocol document (#246).
- Opening the endpoint to the tunnel password.
