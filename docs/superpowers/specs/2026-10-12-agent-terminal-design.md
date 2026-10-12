# Agents at the terminal: design

Issue: to be filed. Builds on
[2026-10-05-mcp-agent-surface-design.md](./2026-10-05-mcp-agent-surface-design.md)
(#243), whose tools were removed in #253 while this was redesigned.
tunnel.pizza's side has its own issue; both link here.

## Intent

`npx tunneld bash` runs a program on a pty and shows it in a browser tab.
The program can be anything: bash, k9s, htop, claude. This design lets an
agent use that same running program headlessly, the way the Bash tool uses
a local shell. It sends a command, watches the output, and reports back. A
person and an agent can be on the same terminal at the same time, and the
person sees everything the agent does.

What Christian decided:

- **The running session.** An agent attaches to the program the run
  started, the one the tab shows. It does not get a process of its own yet.
  A process of its own ("its own pty") is on the roadmap, so its scope is
  defined now and refused until it ships.
- **The agent types freely, the person watches.** Agent input appears live
  in the tab, labeled. Anything the person types wins over the agent, and
  the person can cut the agent off from the tab.
- **Through tunnel.pizza's MCP.** An agent connects to one server,
  `https://tunnel.pizza`, and signs in once. The terminal tools take a
  hostname, and tunnel.pizza's Worker relays each call to that tunnel's
  `/_tunneld/mcp`.
- **Spec and issues before code**, and authentication, authorization and
  `list_tunnels`'s data settled in a first phase before any terminal tool.

## Phases

The work is split so authentication, authorization and what tunnel.pizza
knows about a tunnel are settled before any terminal tool is written.

### Phase 1: reaching a tunnel, and knowing what it serves

Three questions, each answered before Phase 2 starts.

1. **What a tunnel serves, without asking it.** Decided, and built on the
   tunnel.pizza side (tunnel.pizza#66):
   - tunneld sends `X-Tunneld-Origins` (each origin as scheme, host and
     path; credentials, query and fragment dropped) and `X-Tunneld-Host`
     (the machine's hostname, the `HOSTNAME` a run saves) on every mint
     (tunneld#280).
   - tunnel.pizza keeps them in the record's tags file
     (`/data/tunnel/<sha256 of the record id>.json`), refreshed on every
     mint or resume. `list_tunnels` returns `origins` as URLs, with no call
     to the tunnel. A tunnel minted by an older tunneld has none.
   - `get_tunnel(hostname)` is the slow path: a live read of the run's
     `.env` through `/tunnel/env`, the info popover's own route. It answers
     the machine, the working directory, origins with `on: <machine>` for
     `localhost` and `exec:`, visibility, and settings.
   - Agents call `get_tunnel` only when a question needs more than the list
     has. The tool descriptions say so, after an agent asked "what tunnels
     do I have" and read every tunnel live.
   - Both headers are in the October legal batch (tunnel.pizza#18).
2. **Getting through a Password or SSO tunnel.** The Worker reverse-proxies
   to `https://<host>/_tunneld/mcp`. It does not pass the caller's
   Authorization header down as it is: that token's audience is
   tunnel.pizza, and a tunnel the person reaches through someone else's SSO
   list would receive a token it could replay against tunnel.pizza's MCP as
   that person. Two ways through, to decide between:
   - **Token exchange** (RFC 8693, tunnel.pizza#35): the caller's token is
     traded for one with audience `https://<host>/_tunneld/mcp`, the
     consented tunnel scopes and a 5-minute life. tunneld checks it,
     scopes included, and knows which person and agent act. A password
     never opens the control path; SSO works because the exchanged token
     keeps the person's `sub`.
   - **The tunnel's secret**, which tunnel.pizza already reads for
     `/tunnel/env`: the Worker proxies with `Authorization: token <secret>`.
     It works today with no tunneld change, but tunnel.pizza alone enforces
     scope and the SSO list, and tunneld sees only "the operator".

   Decided: token exchange for every terminal tool, read or write (see
   [The relay](#the-relay)). The secret stays only for the reads
   tunnel.pizza's own routes already make (`/tunnel/env`, `/tunnel/ping`),
   which tunnel.pizza's MCP tools reach as the signed-in person.
3. **Asking for consent later, mid-chat.** MCP has two ways for a server to
   ask for more mid-conversation, and the v2 SDK has both: a tool call
   answered with `403` and `WWW-Authenticate: Bearer
   error="insufficient_scope", scope="..."` (step-up authorization;
   `scopeChallenge` on `registerTool`), and URL elicitation
   (`UrlElicitationRequiredError`), which asks the client to open a page.
   Answered by a probe on 2026-10-12, with a `tunnel_attach` placeholder
   needing `tunnel:attach` and Claude Code 2.1.296:
   - **Step-up works once a token exists.** The 403 made Claude Code ask
     "needs additional permissions ... Re-authenticate now?", open
     `/oauth/authorize` with `scope=mcp offline_access tunnel:attach` and
     `prompt=consent`, and retry the call once. It took about 80 seconds.
     So each tunnel scope is asked for the first time a tool needs it, not
     at the first sign-in.
   - **The first sign-in cannot be deferred.** Served without a token, a
     tool call answered with either the 403 or a 401 made Claude Code read
     the metadata, start no authorization, drop the server and send the
     person to `/mcp`. So `mcp` itself is still asked for at connect.
   - URL elicitation was not needed and was not probed.

Phase 1 ends with: `origins` in `list_tunnels` (done, pending tunneld#280),
the way through decided (token exchange) and built, tunneld checking
scopes, and consent asking for each tunnel scope when a tool first needs
it.

### Phase 2: the terminal tools

The agent viewer, the output log, knowing when a command finished, and the
tools below, built on what Phase 1 settled.

## What exists

On main today:

- **One shared session per attached origin** (`attach/session.go`). The
  target's output goes `sink` -> `scanner` -> a charmbracelet `x/vt`
  `SafeEmulator`. Every viewer gets a server-side frame rendered from that
  screen, and nothing is replayed to a late joiner: it sees the current
  screen. The emulator already gives cells, scrollback, the cursor, the
  alternate screen and `transcript()`.
- **Input** from a viewer reaches the program through the emulator's
  `SendKey`, `SendText` and `Paste`. Size is negotiated across viewers.
- **Viewers are anonymous.** `count()` is the only notion of who is there.
- **`/_tunneld/mcp`** is mounted with the Go SDK (v1.8.0), stateless, with
  no tools. It opens to the tunnel's secret, or to a tunnel.pizza access
  token whose audience is `https://<host>/_tunneld/mcp` and whose `sub` is
  the owner or listed in the SSO challenge. Tokens are verified offline
  against tunnel.pizza's keys. No scope is checked.
- **No shell integration.** OSC 133 passes through to the page unhandled.

So the screen model this design needs is already there. What it adds is
an agent viewer with an identity, a cursor-addressed output log, a way to
tell when a command finished, tools, scopes, and the relay.

## Shape

```
agent --MCP--> https://tunnel.pizza              (one server, one sign-in)
                 Worker: edge/mcp/tools/terminal_*.ts
                 token exchange: caller's mcp token -> token for
                   aud https://<host>/_tunneld/mcp, scope tunnel:attach/exec
                 --MCP--> https://<host>/_tunneld/mcp   (tools/call relayed)
                            tunneld: agent viewer on the shared session
                              read:  emulator screen, output log
                              write: SendText / SendKey, as a tab does
                            tab: "claude is typing", the person's keys win
```

## tunneld

### The agent viewer

An MCP tool call that touches a session joins it as a **headless viewer**.
It is the same `*viewer` a tab is, but with no frame, no size vote and an
identity: the token's `sub` and `client_id`. A tab's viewers gain an
identity too (the SSO `sub` when there is one), so presence becomes "who",
not only "how many". The chrome's `viewers` field lists them, and the
tab shows an agent as one line: `claude (Claude Code) is attached`.

An agent viewer lasts as long as calls keep arriving. One idle for 10
minutes is dropped. It never sends a size, so a person's window decides the
size.

### The output log

`terminal_read` in stream mode needs "everything since I last looked". The
emulator holds a screen, not a history of bytes. A per-session **output
log** is added beside it: a ring buffer of the program's output as text,
with ANSI control sequences stripped, each byte at a monotonic offset. A
read passes an offset and gets what came after it, plus the next offset.
When a reader falls behind the ring, it gets what is left and
`dropped: true`. The ring is 1 MiB, the same bound the native-tab tee
uses.

The screen mode reads the emulator directly: the visible rows as text, the
cursor, whether the alternate screen is up, and the title.

### Knowing when a command finished

`terminal_run` sends a line to a shell and returns its output and exit
code. tunneld knows a command finished in one of three ways, best first:

1. **OSC 133 (shell integration).** When the origin is a shell tunneld
   recognises (bash, zsh, fish), it starts it with a small integration that
   emits OSC 133 `A`/`B`/`C`/`D;<exit>` around every prompt and command.
   The scanner already sees OSCs; it records the marks against the output
   log's offsets. A run ends at the next `D`, with its exit code.
2. **A sentinel.** When a shell is in front but has no integration, the
   command is sent with `; printf '\e]7770;done;%s;%s\a' <nonce> $?`
   appended. The scanner keeps OSC 7770 for itself and never shows it.
3. **Quiet.** For anything else, a run ends when the program has written
   nothing for `idle_ms` (default 500).

The result says which one ended it, so an agent knows whether `exit_code`
is real.

### Tools on `/_tunneld/mcp`

| Tool | Scope | Arguments | Result |
|---|---|---|---|
| `terminal_sessions` | `tunnel:attach` | none | `[{origin, n, command, cols, rows, viewers}]` |
| `terminal_screen` | `tunnel:attach` | `n?` | `{rows: string[], cursor: {row, col}, alt_screen, title}` |
| `terminal_read` | `tunnel:attach` | `n?`, `since?` | `{text, next, dropped}` |
| `terminal_wait` | `tunnel:attach` | `n?`, `since`, `until?` (regex), `idle_ms?`, `timeout_ms?` | `{text, next, matched, timed_out}` |
| `terminal_send` | `tunnel:exec` | `n?`, `text?`, `keys?` | `{next}` |
| `terminal_run` | `tunnel:exec` | `n?`, `command`, `timeout_ms?` | `{output, exit_code?, ended_by, next, interrupted}` |

- **`n`** picks the origin, as `?n` does in a URL. It defaults to the first
  attached origin.
- **`keys`** are names, never escape sequences: `Enter`, `Tab`, `Esc`,
  `Backspace`, `Up`, `Down`, `Left`, `Right`, `Home`, `End`, `PageUp`,
  `PageDown`, `F1`-`F12`, and `Ctrl-<letter>`. They go through the
  emulator's `SendKey`, so the program gets what a tab would send in its
  current modes.
- **Waits stay short.** `timeout_ms` defaults to 10 000 and is capped at
  25 000. Cloudflare answers a request that writes nothing for about 100
  s with a 524, and the relay adds its own hop. An agent that needs longer
  loops on `terminal_wait`.
- **Output is bounded.** `text` and `output` are capped at 64 KiB, keeping
  the end and saying so, so a log dump cannot flood an agent's context.
  `terminal_screen` is at most rows x cols.

### The person's keys win

- While an agent's `terminal_run` or `terminal_send` is in flight, a
  keystroke from any tab ends the agent's call. The call returns
  `interrupted: true` with what it had.
- For 2 seconds after a person's last keystroke, agent input is refused
  with a tool error: "Someone is typing in the tab. Try again shortly."
- The tab's command strip gains **Detach agents** (`^K a`). It drops every
  agent viewer and refuses agent tools on that session until the person
  undoes it or the run restarts.
- Agent input shows in the tab as it lands. The chrome's `notice` carries
  `claude is typing` while an agent call is writing.

### Scopes

tunneld starts checking scopes on access tokens at `/_tunneld/mcp`:

| Scope | Allows |
|---|---|
| `tunnel:attach` | the read tools: `terminal_sessions`, `terminal_screen`, `terminal_read`, `terminal_wait` |
| `tunnel:exec` | `terminal_send` and `terminal_run`, on the running session |
| `tunnel:spawn` | defined now, refused until it ships: a process of the agent's own (the 10-05 spec's `exec`, `put_file`, `get_file` and `session_*`, and #245's isolation) |

A token with none of them still opens the endpoint, as today, but every
tool refuses with "this sign-in can't use the terminal" and the scope it
needs. The tunnel's secret keeps opening everything, as the 10-05 spec
decided.

## tunnel.pizza

### Scopes and consent

- The auth library's app config gains `tunnel:attach`, `tunnel:exec` and
  `tunnel:spawn`. Each is gated behind the `mcp` feature, like `mcp`
  itself, and each has its own consent line:
  - "Read what's on <host>'s terminal"
  - "Type into <host>'s terminal as you"
  - "Start programs on the machine running <host>"
- #62 is where these scopes were deferred; this design fills it in.

### Which tunnels: authorization details (RFC 9396)

A scope says what an app may do; it cannot say where. Which tunnels is
carried as RFC 9396 authorization details, chosen on the consent page with
the picker in tunnel.pizza#63:

```json
"authorization_details": [{
  "type": "https://tunnel.pizza/tunnel",
  "actions": ["attach", "exec"],
  "locations": ["https://0tabc1234de.tunneled.pizza"]
}]
```

- **`actions`** mirror the tunnel scopes without the prefix. The scope is
  the coarse grant and the request trigger. The details narrow it. A tool
  needs both: `tunnel:exec` in `scope`, and `exec` for that tunnel in the
  details.
- **`locations`** are tunnel origins, or `["*"]` for "All tunnels". `*`
  means every tunnel the person owns at the time of use, including ones
  minted later. So it is checked against ownership when the token is
  exchanged, never expanded into a list at consent.
- **Clients that send only `scope`**, which includes Claude Code today:
  the consent page still shows the picker, defaulting to "All tunnels".
  The authorization server writes the person's choice into the token's
  `authorization_details` claim (RFC 9396 section 9.1, JWT access tokens)
  and the token response. A client that sends its own details gets the
  picker pre-filled with them, and the person can only narrow them.
- **Refresh** keeps the details granted at consent. A `*` grant follows
  ownership by itself. A listed tunnel the person no longer owns fails at
  exchange, not at refresh.
- **The consent cookie** (`grantOf` / `covers`) remembers the details as
  well as the scopes. A request for a tunnel or an action outside what was
  remembered asks again.
- **`src/auth` stays generic.** The library parses, shows and issues
  authorization details of the types the app registers. tunnel.pizza
  registers `https://tunnel.pizza/tunnel` through `AuthConfig`, along with
  the person's tunnels for the picker, the same way `scopes` and
  `resourcePaths` arrive.

### The relay

- `edge/mcp/tools/` gains one file per terminal tool. Each takes
  `hostname` plus the tool's own arguments.
- A relayed call needs a token tunneld accepts, with audience
  `https://<host>/_tunneld/mcp` and the scopes the person consented to.
  The caller's token has audience `https://tunnel.pizza/`.
- The Worker trades one for the other through **token exchange** (RFC
  8693, tunnel.pizza#35) at the app: the subject token is the caller's,
  the resource is the tunnel's MCP URL, and the requested scopes come from
  the tool's needs. The exchanged token lives 5 minutes, and the Worker
  keeps it in isolate memory until it expires. So a burst of polls costs one
  Lambda call, not one per poll.
- **The tunnel is its own audience, never the caller's token's.** Putting
  tunnels in the client's token (`aud: [tunnel.pizza, <tunnel>]`) would hand
  every tunnel a token that also works at tunnel.pizza, replayable by
  anything on that machine. The client also asks for one resource,
  `https://tunnel.pizza/`, and cannot know hostnames up front. The caller's
  token never leaves tunnel.pizza.
- **What the exchanged token says:**
  - `aud`: exactly one tunnel's MCP URL, `https://<host>/_tunneld/mcp`.
  - `scope`: the tunnel scopes the tool needs, and only those the caller's
    token holds.
  - `authorization_details`: the caller's tunnel entry narrowed to this one
    location and these actions.
  - `sub`: unchanged, the person.
  - `act`: `{"sub": <client_id>}` (RFC 8693 section 4.1), the app acting
    for the person, which tunneld uses to name the agent viewer.
  - `exp`: 5 minutes.
- **The exchange refuses** when:
  - the caller's token lacks the scope (the Worker answers the tool call
    with the 403 step-up instead);
  - its details do not cover this tunnel and action, or the person may no
    longer open the tunnel (owner, or on its SSO list);
  - the sign-in was revoked.

  An exchange never widens audience, scope or details.
- tunneld checks the exchanged token as it checks any access token at
  `/_tunneld/mcp`, offline against tunnel.pizza's keys: `aud` is its own MCP
  URL, the tool's scope is present, `sub` may open the tunnel, and the
  details name its origin (or `*`) with the tool's action.
- The Worker relays `tools/call` to tunneld as an MCP client and returns
  the result unchanged. A tool error from tunneld stays a tool error.
- **Rate limits:** `MCP_PER_IP` (120 per minute) covers the relay too.
  Polling loops fit inside it.

### Legal

An approved app can now read and type into a person's terminal. That is
a new line in the month's legal batch: what the app can do, that it acts as
the person, and that the person sees it in the tab.

## Errors and security

- **Who.** Only a person who could open the tab can approve an agent for it
  (the owner, or someone on the SSO list), and only with the scopes they
  approved. A password-protected tunnel's password never opens the control
  path, as today.
- **What the agent can do.** `tunnel:exec` is typing into whatever runs.
  On a shell, that is the shell's user on that machine. The consent line
  says so plainly.
- **Seeing it.** Every agent keystroke lands in the tab in view. Agent
  calls log one line each (tool, origin, duration, how it ended), never
  the input or output.
- **Stopping it.** The person's typing interrupts. Detach agents cuts all
  of them off. Revoking the sign-in at tunnel.pizza stops the exchange, so
  the relay stops within the exchanged token's 5 minutes.

## Testing

- tunneld, `attach`: an agent viewer joins and leaves with no size vote.
  The output log hands out offsets and reports drops. OSC 133 marks end a
  run with its exit code. The sentinel ends one and never reaches a tab.
  Quiet ends one. A tab keystroke interrupts a run, and agent input within
  2 s of it is refused. Detach agents refuses later calls.
- tunneld, `mcp`: each tool against a real shell origin (`exec:///bin/sh`)
  in the existing e2e style. Scopes refuse what they don't cover.
- tunnel.pizza, `edge/mcp`: each terminal tool relays to a stub tunneld,
  exchanges once per token lifetime, and passes tool errors through.
- tunnel.pizza, `src/auth`: token exchange narrows audience, scope and
  authorization details and never widens them; `act` names the client. A
  `*` grant exchanges only for a tunnel the person owns at the time. Consent
  writes the picker's choice into `authorization_details` for a client that
  sent only `scope`, and asks again for a tunnel or action outside the
  remembered grant.
- tunneld, `mcp`: a token for another tunnel, missing the scope, or whose
  details leave out this origin or action is refused per tool.

## Out of scope, each its own issue

- `tunnel:spawn`'s tools: a process of the agent's own, files, and #245's
  isolation.
- Pushing output to clients as it arrives (MCP `subscriptions/listen`).
  Polling with `terminal_wait` works with every client today.
- The `tunneld --mcp` stdio shim (#249).
- Agents on other programs' sessions when a run has several origins: `n`
  selects them, but the tab's Detach agents and presence are per session.
