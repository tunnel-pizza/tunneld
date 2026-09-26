# Messages of the day — design

Issue: [#141](https://github.com/tunnel-pizza/tunneld/issues/141).
Upstream: cnuss/libtunnel#239 (PR #252, shipped in libtunnel v0.1.10; tunneld
pins v0.1.9 today), tunnel.pizza#8.

## Intent

tunnel.pizza can say something with a mint — a notice, a warning, a caution —
and tunneld shows it wherever an operator or a viewer is looking: the console
the run was started on, the frame every viewer of a served terminal sees, and
the multiview panel. The message is the provider's word about *this* tunnel
as of its mint; it is shown every run, cached spec or fresh, and nobody can
dismiss it. Severity changes colour and label, nothing else.

What Christian said, verbatim where it matters:

- Surfaces: stderr after the origin map; the console frame; the multiview
  panel. Future, not now: script injection into single-port origins.
- Terminal markdown: full rendering through `charmbracelet/glamour`.
- Severity: colour and label only.
- Cached replays: shown every run.
- Frame: "all are drawn at the top of the viewport unconditionally and not
  dismissable, centered text to the viewport".
- Panel: same, a strip above the tiles.
- Shape: a sixth contract, `Motd`, and "console and display would have
  WithMotd and embrace the Motd interface to provide what they need".

Assumptions, for correction: the frame is the binder's (`attach`), so the
attach package is where `WithMotd` lands for the terminal surface; the console
package only shows what attach draws. Rendering to one line per message in the
frame is mine: a banner of constant height per run is what keeps the pane's
size negotiation as it is (section 6).

## What arrives

`Tunnel.Messages() []string` (libtunnel ≥ v0.1.10), nil when the provider said
nothing. Each string is an RFC 2397 data URL, `data:text/markdown;base64,…`,
standard base64 of UTF-8 markdown. Severity is not on the wire: the markdown
opens with a GitHub alert line, `> [!note]`, `> [!warning]` or `> [!caution]`,
and the rest of the message is the blockquote under it. Order is most severe
first as the server sent them. Messages ride libtunnel's envelope beside the
spec, so tunneld's cache file replays them with the spec they came with.

## 1. Contract

`v1alpha1/v1alpha1.go` gains a sixth contract, declared after `Identity`, with
the same treatment as the other five: a field on `BuilderImpl`, `WithMotd`, a
default in `New`, a line in the assertion block and in `Command`'s wiring
check, a fake and a `TestRun` case, a file-map row in CONTRIBUTING.

```go
// Motd is what the provider said with the spec, kept for every surface that
// shows it. Learn takes the strings as libtunnel hands them over; the rest
// answer from what was learned, and answer nothing until then.
type Motd interface {
    Learn(raw []string, log v1.Logger)
    Print(w io.Writer, width int)
}
```

`Print` is the builder's own use. The other surfaces do not name the contract:
attach declares `Motd interface{ Lines(width int) []string }` beside its `Logs`
interface, and display declares `Motd interface{ HTML() []motd.Rendered }`.
`motd.MotdImpl` satisfies all three. The narrow interfaces are the pattern
`attach.Logs` set: a package names what it reads, not who provides it.

One instance, built before the builder the way the log ring is, and shared:

```go
board := motd.New()
b := v1.Apply(&BuilderImpl{recent: recent, /* … */},
    // …
    WithMotd(board),
    WithDisplay(display.New(display.WithMotd(board))),
    WithBinder(attach.New(
        attach.WithTargets(docker.New(), shell.New()),
        attach.WithBanner(VersionLine(nil)),
        attach.WithLogs(recent),
        attach.WithMotd(board),
    )),
)
```

An embedding program that replaces the Motd replaces it in all three places,
as it does the ring today.

## 2. Package `v1alpha1/motd`

`MotdImpl`, `New(opts ...Option)`, `type Option = v1.Option[*MotdImpl]`. No
knobs yet; the variadic is there because every package takes it.

```go
type Severity string // "note" | "warning" | "caution" | "" (none)

type Message struct {
    Severity  Severity
    MediaType string // from the data URL, "text/markdown" today
    Body      string // markdown with the alert line and the "> " prefixes removed
}
```

State: `messages []Message` behind an `atomic.Pointer`, written once by
`Learn`, read by every renderer. `Learn` before any viewer can exist — a
viewer needs the hostname, the hostname needs the mint, the mint is what
carries the messages — but the pointer makes the detector agree.

`Learn(raw, log)`: for each string, `parse` (below); a string that does not
parse is logged at warn with its index and dropped, the rest are kept in
order. A nil or empty `raw` leaves no messages.

`parse(s string) (Message, error)`:

1. Must start with `data:`. Split at the first `,` into header and payload.
2. Header: media type is the part before the first `;`, defaulted to
   `text/plain` when empty (RFC 2397 default); parameters after it are read
   only for `base64`. Without `;base64`, the payload is percent-decoded.
3. Payload: standard base64 (padding tolerated), then must be valid UTF-8.
4. If the media type is `text/markdown`: when the first line matches
   `^>\s*\[!(note|warning|caution)\]\s*$` (case-insensitive), that line is the
   severity and is removed; then every remaining line has one leading `>` and
   one optional space stripped. Any other first line leaves severity empty and
   the body as decoded. Other media types keep the body as decoded and have no
   severity.

Renderers, each reading the learned messages:

- `Print(w io.Writer, width int)` — stderr. Per message: a label line
  (`NOTE`, `WARNING`, `CAUTION`, or none) then the body through glamour
  (`glamour.WithAutoStyle()`, `glamour.WithWordWrap(width)`), trailing blank
  lines trimmed. `width` is the terminal's, or 80 when the writer is not a
  terminal; with no terminal the label carries no colour and the body is
  printed as decoded, not through glamour. A glamour error prints the body as
  decoded. Nothing is printed when there are no messages.
- `Lines(width int) []string` — the frame. One string per message: label,
  a space, and the body flattened — newlines to spaces, runs of spaces
  collapsed, markdown left as text — then truncated to `width` with a
  trailing `…` when it does not fit. Styled with the severity's colour
  through `ansi` so `uv.NewStyledString` draws it. Centering is the frame's.
- `HTML() []Rendered`, `Rendered{Severity Severity; HTML template.HTML}` — the
  panel. Body through goldmark (`goldmark.New()` defaults: raw HTML escaped,
  no unsafe option) with a link renderer that adds `target="_blank"
  rel="noopener"`. A non-markdown media type is HTML-escaped and wrapped in
  `<p>`.

Colours: note cyan, warning yellow, caution red, none plain. Label text is
the severity upper-cased. One place defines both, used by all three
renderers.

## 3. stderr

In `Command`'s run, after the `-> origin` lines and before the console/browser
hand-off and the stop hint:

```go
b.motd.Learn(tun.Messages(), log)
b.motd.Print(stderr, widthOf(stderr))
```

`Learn` sits here because `tun.Messages()` resolves the spec, which is already
resolved by `tun.URL()` returning. The output contract in the README gains the
messages as the third thing stderr carries.

## 4. Frame

`attach.BinderImpl` carries `motd Motd` from `attach.WithMotd`, hands it to
`Serve`, and the session keeps it. The frame draws the banner at the top of
its window: `f.sess.motd.Lines(f.width)`, row `i` at `y = i`, each centred by
its display width in `f.width`. Always drawn — on the live screen, scrolled
back, in the logs view, the QR view and command mode — and no key touches it.

The box moves down by the banner's height: `box()` centres in the window
below the banner, so the pane and the corner chips sit where they do today
relative to the box.

## 5. Panel

`display.DisplayImpl` carries `motd Motd` from `display.WithMotd`. `pageData`
gains `Messages []motd.Rendered`, filled per request from `motd.HTML()` — per
request rather than at interceptor registration, because the interceptors are
registered before the mint. The template puts a `<header class="motd">`
before `<main class="grid">`, one `<div class="message message-{{.Severity}}">`
per message, text centred, colour by severity from Basecoat's tokens (the
same four hues as the terminal). No message, no header element at all. The
grid takes the remaining height, as it does under any header.

## 6. Size negotiation

The banner is one row per message, so its height is `len(messages)` for the
whole run and the same for every viewer. `paneOf(window)` subtracts it along
with `chromeHeight`, from a `banner int` the session knows; `box()` and
`pane()` place themselves under it. The smallest-viewer rule is unchanged: a
viewer's pane is its window less the same chrome as everybody's. A window
shorter than chrome plus banner keeps the one-row pane floor it has today.

## 7. Errors

- A message that does not parse: warn, skip. The others still show.
- glamour or goldmark failing on a body: fall back to the body as text.
- `Messages()` nil: nothing printed, zero banner rows, no panel header.
- A media type other than `text/markdown`: shown as text with no severity;
  the type is kept on the Message for whoever asks later.

## 8. Tests

- `motd/motd_test.go`: parse table — note/warning/caution, no alert line,
  unknown alert, nested `>` lines, no `;base64`, bad base64, non-UTF-8,
  `text/plain`, not a data URL, empty input, order kept; `Lines` truncation
  and flattening; `HTML` escapes raw HTML and opens links in a new tab;
  `Print` with and without a terminal.
- `attach/frame_test.go`: banner rows drawn centred above the box, the pane
  shorter by their count, no key clears them, a scrolled/logs/QR view keeps
  them.
- `attach/session_test.go`: `paneOf` with a banner.
- `display/display_test.go`: rendered page carries the header with one
  element per message and escapes raw HTML; no header without messages.
- `builder_test.go`: `TestRun` case with a fake Motd — stderr carries the
  label and body after the origin lines; `Learn` was called with what the
  fake tunnel's `Messages()` returned.
- Live: a mint against tunnel.pizza on the new libtunnel, to see what it
  sends today and how it reads on all three surfaces.

## 9. Delivery

Two PRs. First `chore: libtunnel v0.1.11` (scratch build compiles, vet clean;
v0.1.10 is the Spec-package refactor and v0.1.11 a dependency roll-up).
Then the feature, `Closes #141`. Docs in the feature PR: README output
contract and panel sections, a keys-table note that the banner takes no key;
CONTRIBUTING file map, contracts list and "Adding a collaborator";
CLAUDE.md's "five internal contracts" → six; the `contracts-and-options`
memory.

## Out of scope

- Script injection into single-port origins (Christian: "future").
- Any way to silence a message.
- Severity doing more than colour and label.
- A markdown renderer that knows GitHub alerts: the alert line is parsed
  here and the body is ordinary markdown to glamour and goldmark.
