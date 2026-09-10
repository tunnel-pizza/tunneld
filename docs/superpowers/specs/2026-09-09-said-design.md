# What an app says about itself is read before the screen sees it

Design for [#93](https://github.com/tunnel-pizza/tunneld/issues/93) and
[#66](https://github.com/tunnel-pizza/tunneld/issues/66). Written 2026-09-09,
against tunneld v0.0.39 (0621940, `feat: a run can be told to show nothing, and
shows its logs instead`).

## The problem

A container's output reaches the emulator unexamined. `sink.Write` in
`v1alpha1/attach/session.go` is `s.em.Write(p)` and a wake, and everything the
frame knows about the program on the other side — its title, whether it hid
the cursor, its working directory — arrives through `vt.Callbacks`, installed
by `session.watch`, plus one raw `RegisterOscHandler(0, …)` beside them for a
distinction the callbacks flatten.

Three things follow from going through the emulator, and #93 names them.

**The data is cut.** `x/ansi`'s parser ends every string state — OSC, DCS,
SOS, PM, APC — at a raw `0x9C` byte, the 8-bit form of the String Terminator.
`0x9C` is also a UTF-8 continuation byte: the second byte of every character in
U+2700–U+273F, and of `“` (`E2 80 9C`) and `末` (`E6 9C AB`). A title carrying
one is cut mid-character, the callback receives `\xe2`, and the rest of the
title is written to the screen as text. The first half is worked around by
`session.remember`, which keeps the last valid title rather than blanking the
label in time with somebody's spinner. The second half is #66, and it is not
reachable from a callback: by the time one fires the residue is already on the
screen. Filed upstream as charmbracelet/x#848 (2026-04-22); PR #946 fixes it
and has sat unreviewed since 2026-08-13; `x/ansi` v0.11.8 still carries
`parser/transition_table.go:269`.

**The set is what we asked for, and only that.** A viewer sees the emulator's
screen buffer, so an OSC that sets no cell reaches nobody. OSC 52 is the
clipboard, OSC 7 is the working directory, OSC 9/99/777 are notifications, OSC
133 is shell integration. `RegisterOscHandler` can tap any of them, but each is
a code to name and the data arrives cut.

**The callbacks are not a place to stand.** They fire inside the emulator's
write, with its lock held, which is a lock-order rule every one of them has to
obey (`watch`'s comment spells it out). They cannot tell one OSC 0 from an
OSC 1 and an OSC 2. And they carry whatever the parser handed them, which is
the cut. Nothing built on them can be trusted further than the parser.

## Decisions

Each was chosen deliberately; the alternative is named so a later reader can
tell a decision from an accident.

**tunneld reads the stream before the emulator does.** A scanner sits in
front of `em.Write` and owns every OSC. Rejected: a `replace` directive
pinning `x/ansi` to PR #946 — the root fix, and one line, but a fork pin in
every release build, and it fixes only the cut: nothing reaches a viewer that
did not before. Also rejected: `RegisterOscHandler` for each code we want —
the data is still cut and the handler still runs under vt's lock.

**The scanner is byte-level and 7-bit only.** It acts on `0x07`, `0x18`,
`0x1A`, `0x1B` and ASCII after ESC, and on nothing else. Every byte at or
above `0x80` is data in every state, which is what makes `✳` survive and is
exactly xterm's rule in UTF-8 mode: C1 controls are recognised only as
U+0080–U+009F, never as a raw byte. An app that sends 8-bit introducers
(`0x9D` for OSC, `0x9C` for ST) is unsupported — they were never valid in a
UTF-8 stream. Rejected: decoding UTF-8 in the scanner, which buys nothing over
never treating a high byte as control.

**OSC is owned; private modes are observed; everything else passes.** The
emulator receives no OSC unless a sink hands it one. `CSI ? … h|l` is written
to the emulator *and* reported, because the frame needs DECTCEM and the
callbacks were the only source. Every other CSI passes through unreported: SGR
and cursor movement are every redraw, and painting is the emulator's job.
DCS, SOS, PM and APC pass through and are tracked only so that `ESC ]` inside
a Sixel or kitty-graphics payload is not mistaken for an OSC. Rejected:
reporting every CSI (volume, for nothing that wants it) and owning CSI (the
emulator is the screen).

**One contract, fan-out, installable.** `attach.Sink` is told every owned or
observed sequence, in order; the session's own sinks come first and
`attach.WithSinks` appends more. Each sink routes for itself. Rejected: a
method on `Bound`, which would couple the run's contract to terminal internals
for a caller that has nothing to do with them; and a single `switch` in the
session, which cannot be inspected from outside.

**vt's callbacks go entirely.** `SetCallbacks` is not called. What they
supplied and the frame used — titles and cursor visibility — comes from the
sinks. What they supplied and only the log used — bell, alt screen, cursor
style, colours, modes — comes from the `logged` sink where it is a sequence
(colours, modes) and is gone where it is not (bell is a C0 byte, cursor style
is `CSI SP q`). Rejected: keeping `CursorVisibility` alone, which keeps the
lock-order rule alive for one line.

**The emulator sees an OSC only if it paints or answers.** OSC 8 becomes
cell links the frame draws; OSC 10/11/12 and 110/111/112 set colours and
answer `?` queries into stdin. That list is vt's `registerDefaultOscHandlers`
minus what we own (0, 1, 2, 7), and a vt bump has to re-check it. Rejected:
passing everything vt registers, which puts 0/1/2/7 back through the cut and
the residue back on the screen.

**Forwarding is per viewer, buffered, and drops when full.** The rule `wake`
already follows: a viewer that has stopped reading holds nobody up.
Rejected: `prog.Send` under `mu` (blocks the stream on the slowest viewer) and
a goroutine per sequence (unbounded).

**OSC 52 works in both directions.** A write is forwarded; a `?` is forwarded
and marks a read outstanding; a reply is accepted only while one is, within a
grace, and the first one wins. Christian chose both directions over writes
only, knowing the read prompts per hostname (#57).

**An unterminated OSC is bounded at 1 MiB.** Past that it is not a sequence:
the held bytes are released to the emulator as they are and the scanner goes
to ground. Large enough for any clipboard write a terminal would accept, small
enough that a stream that never terminates costs one buffer.

## The scanner

`v1alpha1/attach/scan.go`. A `scanner` is an `io.Writer` with a `screen
io.Writer` (the emulator), a `said func(Sequence)` (the fan-out), a `held
[]byte`, and a state.

| State | Byte | Action | Next |
|---|---|---|---|
| ground | ESC | hold | esc |
| ground | other | → screen | ground |
| esc | `]` | hold | osc |
| esc | `[` | hold | csi |
| esc | `P` `X` `^` `_` | release held + byte → screen | string |
| esc | other | release held + byte → screen | ground |
| osc | BEL | complete | ground |
| osc | ESC | hold | oscEsc |
| osc | CAN, SUB | discard held | ground |
| osc | other | hold (cap: release → screen, ground) | osc |
| oscEsc | `\` | complete, ST included | ground |
| oscEsc | other | complete without ST; re-enter esc with this byte | esc |
| csi | `0x30`–`0x3F` (params), `0x20`–`0x2F` (intermediates) | hold | csi |
| csi | `0x40`–`0x7E` (final) | release held + byte → screen; report if `?`-prefixed and final is `h` or `l` | ground |
| csi | other | release held + byte → screen | ground |
| string | ESC | hold | esc |
| string | other | → screen | string |

"Complete" delivers one `Sequence` whose `Raw` is the held bytes, introducer
through terminator, and clears `held`. "Re-enter esc with this byte" is
xterm's rule for an ESC that is not the first half of ST: the OSC ends where
it is and the ESC starts whatever comes next. In `string`, ESC hands over to
`esc` because that state already knows what to do with `\` (release; the
string is over) and with `]` (the string is over and an OSC begins).

Ground bytes are written to the screen as they arrive, so a chunk that is all
text costs one write and no copying, and nothing waits on a sequence that is
still coming. Only a trailing ESC, an open OSC and an open CSI are held.
`revive` calls `reset`, which drops `held` and returns to ground — a run can
die mid-sequence and the next one must not inherit half of it.

The scanner is pure: it does not know what a sequence means and it never
writes an OSC to the screen. A sink that wants the emulator to have one writes
it there itself.

## The contract

In `v1alpha1/attach/attach.go`, beside `Target`, `Logs` and `Repeatable`:

```go
// Sink is told what a program said about itself, as it said it.
type Sink interface {
	Said(seq Sequence)
}

// Kind is which of the two shapes a Sequence is.
type Kind int

const (
	OSC  Kind = iota // owned: the emulator gets it only from a sink
	Mode             // observed: CSI ? Pm h|l, already on its way to the emulator
)

// Sequence is one thing a program said, exactly as it said it.
type Sequence struct {
	Raw  []byte // introducer through terminator
	Kind Kind
	Cmd  int    // OSC: the number before the first ';'. Mode: the mode number
	Data []byte // OSC: everything after the first ';', without the terminator
	Set  bool   // Mode: h rather than l
}
```

An OSC with no leading digits has `Cmd` -1 and `Data` equal to the payload;
the `screen` sink passes it to the emulator, since it is not ours to judge. A
`CSI ?` with several modes (`?1049;25h`) is reported once per mode, in order,
each with the same `Raw`.

Delivery is synchronous, in registration order, on the goroutine that read
the stream, and never under the emulator's lock: an OSC is delivered instead
of being written, and a Mode is delivered after its write has returned. A
sink may take `s.mu`. A sink must not block: the container's output is behind
it.

`attach.WithSinks(sinks ...Sink) Option` on `BinderImpl` appends to every
session that binder starts, after the built-ins, the way `WithLogs` and
`WithBanner` reach a session today. `Raw` and `Data` are the scanner's
buffers and are valid only for the call; a sink that keeps one copies it.

## The sinks

Two predicates in `session.go`, shared so there is one table:

```go
func names(cmd int) bool  { return cmd == 0 || cmd == 1 || cmd == 2 }
func paints(cmd int) bool { switch cmd { case 8, 10, 11, 12, 110, 111, 112: return true }; return false }
```

| Sink | Takes | Does |
|---|---|---|
| `titles` | `names` | `setTitle`/`setSubtitle`, whole; 0 sets both |
| `cursor` | Mode 25 | `setCursorHidden(!Set)` |
| `screen` | `paints`, and `Cmd == -1` | `em.Write(Raw)` |
| `viewers` | every OSC with `Cmd` ≥ 0 that is neither `names` nor `paints` | forward to each viewer; a `52;…;?` stamps `clipboardAsked` |
| `logged` | everything | one debug line per sequence: kind, cmd, data |

`titles` replaces `remember`'s guard with a plain setter: what arrives is
whole. `showable` in `frame.go` keeps `strings.ToValidUTF8` as the last
defence against an app that genuinely sends garbage.

`paints` is the maintenance point and CONTRIBUTING says so: vt's
`registerDefaultOscHandlers` minus `names` and 7. A bump that adds a handler —
OSC 4 palette, say — is a code the emulator has started painting and this
table is silently withholding.

## Forwarding

`viewer` gains `said chan []byte` (buffered, 64). `redraw` — the goroutine
that already turns `wake` into `paneMsg` — drains it: `v.prog.Send(tea.RawMsg{Msg: string(raw)})`.
Bubble Tea's `RawMsg` handler writes to its output buffer, flushed on the
renderer's next tick; an OSC carries no cells, so it may land anywhere between
two frames. The `viewers` sink snapshots the viewer set under `mu` and sends
to each channel with a non-blocking send; a full channel is a drop and a debug
line.

The console viewer (`viewLocally`) is a viewer with a real terminal for
output, so it receives the same forwards and the terminal does what terminals
do with them. Nothing distinguishes it.

## The clipboard reply

The path exists end to end and only the frame's side is new:

1. `viewers` forwards `ESC]52;<sel>;?BEL` and sets `s.clipboardAsked = time.Now()`
   under `mu`.
2. xterm's clipboard addon calls `navigator.clipboard.readText()` and answers
   with `terminal.input("\x1b]52;<sel>;<b64>\x07", false)`, which fires
   `onData` and lands on the STDIN channel.
3. The frame's Bubble Tea input decodes it (`ultraviolet/decoder.go` case 52)
   into `tea.ClipboardMsg{Content, Selection}`, `Content` already
   base64-decoded.
4. `frame.Update` gains `case tea.ClipboardMsg:` → `f.sess.clipboard(msg.Selection, msg.Content)`.
5. `session.clipboard`, under `mu`: if `clipboardAsked` is zero or older than
   `s.clipboardGrace` (a field, 30s from `newSession` — a permission prompt is
   human time — and short in a test), drop and log at debug. Otherwise clear it and write `ansi.SetClipboard(sel, content)` to
   `s.stdin` — bytes, not keys, on the same pipe the emulator's own colour
   replies use.

First answer wins: N viewers produce N replies to one query and the container
gets one. An unsolicited reply — any viewer's terminal pushing an OSC 52 at a
container that did not ask — is dropped, which is the guard that makes both
directions acceptable.

## The page

`index.html` pins `https://cdn.jsdelivr.net/npm/@xterm/addon-clipboard@0.2.0/lib/addon-clipboard.js`
with a sha384 computed from the bytes jsDelivr serves, like the three pins
above it, and loads it after `open`: `term.loadAddon(new ClipboardAddon.ClipboardAddon())`.
The comment beside it says what now arrives raw: OSC 52 both ways, handled;
7, 9, 133, 777 and the rest, which xterm logs as unknown and ignores until the
page grows a handler. `readText` prompts once per hostname, which is #57's
scope and the decision taken here.

## Layout

| File | Change |
|---|---|
| `v1alpha1/attach/scan.go` | new: `scanner`, states, `reset` |
| `v1alpha1/attach/scan_test.go` | new |
| `v1alpha1/attach/attach.go` | `Sink`, `Sequence`, `Kind`, `WithSinks`; `BinderImpl.sinks` threaded to `newSession` |
| `v1alpha1/attach/session.go` | `watch` → builds the scanner and the five sinks; `sink.Write` → scanner; `viewer.said`; `redraw` drains it; `remember` → plain setter; `clipboardAsked`, `clipboard`; `revive` resets the scanner; `SetCallbacks` and the raw OSC 0 handler removed |
| `v1alpha1/attach/session_test.go` | rewritten cases, new cases |
| `v1alpha1/attach/frame.go` | `case tea.ClipboardMsg` |
| `v1alpha1/attach/frame_test.go` | comment on the `\xe2` cases; `ClipboardMsg` case |
| `v1alpha1/attach/index.html` | the addon pin and load |
| `CONTRIBUTING.md` | the bullet, see below |

## Tests

`scan_test.go` is a table, and every case is run three ways: whole, one byte
per `Write`, and at every two-way split. Each case states what the screen
received and what was said, and a conservation check holds for all of them
but the CAN abort: screen bytes plus owned sequences equals the input, and
for the abort the discarded bytes are exactly the difference.

- `✳ Claude Code` as OSC 0 via BEL, and via `ESC \`; `“` (`E2 80 9C`); `末`.
- A DCS and an APC whose payload contains `ESC ]`: passed through, nothing said.
- ESC as the last byte of a chunk; ESC followed by `[`; ESC alone followed by text.
- An OSC ended by a bare ESC then `[31m`: the OSC is said, the SGR reaches the screen.
- CAN inside an OSC: nothing said, nothing of it on the screen.
- The cap: 1 MiB + 1 of payload with no terminator reaches the screen as-is.
- A raw `0x9C` in ground: on the screen. A raw `0x9C` inside an OSC: in `Data`.
- `CSI ?25l`: on the screen and said as Mode 25 unset. `CSI ?1049;25h`: said twice.
- `CSI 31m`: on the screen, nothing said.
- An OSC with no digits: said with `Cmd` -1.

`session_test.go`:

- `TestATruncatedTitleIsIgnored` becomes `TestATitleArrivesWhole`: OSC 0
  `✳ Claude Code` after a drawn row; both names are `✳ Claude Code` and the
  row beneath the UI is blank — #66's repro as a test.
- A recorder installed with `WithSinks` sees titles, a mode and a forwarded
  OSC in the order the target wrote them.
- OSC 8 around text: the cell carries the link. OSC 0: the emulator's own
  title state stays empty.
- `?25l` then `?25h`: `cursorHidden()` follows.
- A websocket viewer: the target writes `ESC]52;c;aGk=BEL`; a STDOUT frame
  carries exactly those bytes; an OSC 0 written beside it never appears raw.
- The reply: target writes `52;c;?`; `writeFrame(STDIN, ESC]52;c;aGk=BEL)`;
  the target's stdin receives `ESC]52;c;aGk=BEL`. Then: no query → nothing; a
  second reply → nothing; a reply after the grace (the constant is a field the
  test shortens) → nothing.

`frame_test.go`: the `\xe2` entries in `TestABlankTitleLeavesTheBorderWhole`
stay, with the comment rewritten — they are no longer what arrives, they are
what `showable` still refuses. A `ClipboardMsg` reaches `sess.clipboard`.

Live, Christian driving: a Claude Code container's title whole in the border
and in the tab; `printf '\e]52;c;%s\a' "$(printf hi | base64)"` in the
container puts `hi` on the browser's clipboard; `printf '\e]52;c;?\a' && cat`
prompts and prints the reply.

## Surface changes

`v1alpha1/attach` exports `Sink`, `Sequence`, `Kind`, `OSC`, `Mode` and
`WithSinks`. Internal tier; the README's API block is `v1` and does not
change. `index.html` carries one more CDN pin. Nothing at the command line
changes, so `e2e` does not.

CONTRIBUTING, in the attach section: **"`x/vt` ends an OSC string at a
`0x9C` byte"** goes, replaced by one bullet — what an app says about itself is
read before the screen sees it: the scanner, `Sink`, the table, the `paints`
check on a vt bump, 8-bit C1 unsupported, the cap. **"Everything the terminal
says about itself is in the debug log"** is rewritten around the `logged` sink
and loses its last sentence about the emulator's lock. **"The shell's title is
caught, not guessed"** keeps its first half (which OSC is shown and why) and
loses the second (`titleMu` and the two lock orders) — `titleMu` stays, because
the stream goroutine writes what the frame's goroutine reads, but the emulator
is no longer part of that story. "A terminal has a title and a subtitle" and
"A title is arbitrary text" are unchanged. `#65`'s bug 1 gets a comment: filed as
charmbracelet/x#848 with PR #946 open, the module is `x/ansi` not `x/vt`, and
it no longer reaches tunneld.

## Verification

`go test ./... -race -count=1`, e2e included. `scan_test.go`'s three feeding
modes are the chunk-boundary proof. The live run above is the end-to-end
proof, and the one part of it a unit test cannot stand in for is the browser's
clipboard prompt.

## Out of scope

- Page-side handlers for OSC 7, 9, 99, 133, 777: they arrive now; what the
  page does with them is its own change.
- OSC 4 / 104 palette: vt does not paint it today, so it is forwarded like
  anything else; if a vt bump changes that, `paints` changes with it.
- #57's per-tile permission scoping.
- The upstream fix. #946 gets a nudge; nothing here waits on it.
- 8-bit C1 introducers.
