# A native terminal in the tab — design

Issue: [#274](https://github.com/tunnel-pizza/tunneld/issues/274).
Related, closed: #240, #241, #139, #137. Related, open: #57, #261.

## Intent

In a browser tab, a tunneld terminal should feel like VS Code's or Warp's:
selection and copying are the browser's own and instant, history is there to
scroll and search, and the program's output is drawn once, by xterm, not
rendered by the server and shipped as a screen. The frame's own text (the
address, the chips) is ordinary page content a person can select, click or
copy. On the console the frame stays what it is, with its own selection
brought up to a terminal's (word, line, extend) and its scrollback mode
borrowing what k9s's logs view does well. One PR.

What Christian said, verbatim where it matters:

- "it feels home grown where vscode and warp feels more natural"
- all three approaches, "all in one PR please"
- the tab's xterm shows "the program's own bytes" (option a over the
  server-rendered pane)
- "feel free to use k9s logs view as inspiration"
- "the bottom left menu is not set in stone if it needs to change for this"

Assumptions, for correction: the page stays embedded in tunneld and served by
it, as today; serving it from tunnel.pizza (#261) stays a separate change.
The console's frame keeps asking for the mouse as it does now; the tab is
where the mouse goes back to the terminal.

## What exists

- A tab runs the same Bubble Tea `frame` as the console, over a websocket in
  k8s `remotecommand` framing (channels STDIN 0, STDOUT 1, STDERR 2, ERROR 3,
  RESIZE 4; `index.html:171`, `attach.go:820`). The frame asks xterm for the
  mouse (`frame.go:828`), draws the pane from the session's emulator
  (`drawPane`, `session.go:1184`) and its chrome around it, and does its own
  drag-select, copied on release through OSC 52 (`clipboard.go:34`).
- The session's emulator is `*vt.SafeEmulator` (`charmbracelet/x/vt`) with
  a 10,000-line scrollback nobody serializes. The program's stream goes
  through a scanner (`scan.go`) that writes screen bytes to the emulator and
  reports OSCs and private modes to `said` (`session.go:543`); titles are
  kept, paint OSCs go to the emulator, the rest are forwarded raw to each
  viewer (`forward`, `session.go:610`). Nothing is replayed to a joiner.
- The page already draws two things as HTML: the notice line
  (`index.html:160`) and the "detached / ended" overlay (`index.html:162`).
- Multiview tiles are iframes of this page at `/attach/embedded`
  (`display/multiview.html:227`), each with its own websocket.
- The commands menu is `^K` then a key (`frame.go:673`): `d` detach, `x`
  exit, `r` restart, `l` logs, `q` qr, and on the console `[` scrollback
  mode and `m` mouse.

## 1. The tab: the program's bytes, and chrome in HTML

A tab no longer runs the frame. Its websocket carries, on STDOUT:

1. **Chrome state**, as a private OSC the page handles: `OSC 7770 ; <JSON>
   ST`, JSON in standard base64 so the payload is free of controls. Sent
   first, and again whenever a field changes.
2. **A snapshot** of the terminal as it is at that instant (section 2).
3. **The program's stream**, byte for byte from then on (section 3).

xterm renders all three. Nothing else is drawn into it: no border, no title
row, no bottom row.

**Chrome state.** `{origin, address, embedded, title, subtitle, banner,
host, viewers, cols, rows, motd: [{severity, text}], restartable,
notice}`. Each maps to what the frame draws today (`titleLabel`,
`subtitleLabel`, `where`, `banner`, `meta`, `drawBanner`), from the same
session fields. `motd[].text` is the message's plain text with its alert
line removed; the page links `https://` spans itself. `notice` is what the
template puts in `.degraded` today.

**The page** lays out, as HTML around `#term`: a top bar (origin left,
title centred, address right as a real `<a>` the person can click, select
or copy), the motd strips above it (not when embedded), a bottom bar
(banner left, host · viewers · cols×rows right, and a `copied` chip when the
page copies) and a command strip. The address is a link that opens in a new
tab on click and is plain text otherwise. When embedded, the top-right is
the ↗ pop-out instead and there is no bottom bar, as now.

**The command strip** replaces `^K` as the menu's home in the tab: buttons
for **Copy all**, **Save**, **Search**, **Logs**, **QR**, **Restart** (when
restartable), **Detach**, **Exit**, each with its key shown. `^K` still opens
it (the page already intercepts `^K`, `index.html:434`); the same single
keys work with it open; `Esc` closes it. Nothing typed with it open reaches
the program.

- **Copy all** and **Save** use xterm's serialize addon: the whole buffer as
  text to the clipboard, or downloaded as `<origin>-<time>.txt`. Both run
  inside the click, so the clipboard write is a user gesture.
- **Search** opens xterm's search addon bar: a text box, regex and
  case toggles, match count, Enter/Shift-Enter for next and previous,
  `Esc` to close. Matches are highlighted in the buffer. This is k9s's `/`
  filter as a browser does it, over the whole history.
- **Logs** fetches `GET /attach/logs` (tunneld's own lines, `logs.Log`) and
  shows them in an overlay `<pre>`; **QR** shows `GET /attach/qr.svg` (the
  address, from `qr.go`) in an overlay. `Esc` or a click closes either.
- **Restart** is `POST /attach/restart`, **Exit** is `POST /attach/exit`.
  Both are behind the same gate as `/attach`, answer 204, and do what `r`
  and `x` do in `commanded` (`session.restart()`; ending the run). Exit asks
  once, in the page, before posting.
- **Detach** closes the websocket; the page then shows its existing
  "detached" overlay.

**Keys.** xterm encodes them, and the page sends the bytes on STDIN; the
server writes them straight to the program's input, the writer the
emulator's key callback uses today. No decoding on the server. Bracketed
paste, application cursor keys and the mouse are xterm's to encode, because
it sees the program's mode requests in the stream (section 3). The frame's
arming of `^C` and `^D` (`frame.go:141`) moves to the page: the first press
of either, when the strip is closed, shows the chip "^C again to send it" for
the same grace and sends nothing; the second within the grace sends it.

**Selection and copy** are xterm's: drag, double-click a word, triple-click
a line, Shift-click to extend, the wheel and drag past the edge through
history, Cmd/Ctrl+C, the right-click menu. Copy-on-select is off. The frame's
selection code is not used by a tab.

**Size.** The page sizes xterm to the chrome's `cols`×`rows`, the shared pane
size the session negotiates (`negotiate`, `session.go:849`), and centres it,
as the box is centred today; a window larger than the pane gets margin, not
a bigger terminal. The page still sends RESIZE with its own window's size so
the session can negotiate from it. `data-rows`, `data-cols`, `data-cell-w`,
`data-cell-h` keep being set for the multiview panel.

**Cursor.** The program's; xterm draws it. The frame's "no cursor in this
state" rule has no tab state left to apply to.

## 2. The snapshot

A new viewer, or one whose stream was cut (section 3), is caught up with
one write that leaves its xterm as the emulator is: `snapshot(em,
modes) []byte`.

- A full reset (`ESC c`), so the page can also use it to recover.
- The main screen's scrollback, oldest first, then the main screen's rows,
  each cell styled with SGR as it differs from the last (foreground,
  background, bold, faint, italic, underline and its style, blink, reverse,
  strikethrough), hyperlinks as OSC 8 spans, wide characters once with
  their tail skipped, trailing blank cells of a row as a newline. Rows that
  the emulator wrapped stay joined, so a copy of a long line is one line.
- If the program is on the alternate screen: the above for the main screen,
  then `CSI ?1049h` and the alternate screen's rows the same way.
- The cursor's position (`CUP`) and visibility (`DECTCEM`), and every private
  mode the scanner has reported set and not since reset: the mouse modes
  (9, 1000–1003, 1006), bracketed paste (2004), application cursor keys (1),
  focus reporting (1004). The session records them all as `said` sees them
  (`setMouse` becomes a map of every mode), reset with `resetModes`.
- The window title, as OSC 2.

Taken on the stream goroutine between two chunks, under the screen lock, at
the moment the viewer's tee is registered, so the stream the viewer gets
after it is exactly what the emulator consumed after it. Bounded: the
scrollback is 10,000 lines, so a snapshot is at most a few hundred KiB; it
is sent as one STDOUT frame.

The snapshot gives a viewer the history every other viewer has: what the
console shows in scrollback mode, the tab now scrolls to. It is the same
trust as the live screen, which every viewer sees.

## 3. The tee

Each tab viewer has a bounded queue of raw chunks, fed on the stream
goroutine right after the scanner has written a chunk to the emulator.
Everything the program wrote is forwarded, OSCs included: titles (xterm sets
`document.title`), OSC 52 (the clipboard addon), OSC 8, the mode requests the
page needs xterm to see. The frame's `forward` of unknown OSCs is not used
by a tab.

The queue is 1 MiB. A viewer that falls behind it (a tab in a background
window, a slow link) is not fed stale bytes and never loses a byte quietly:
its queue is dropped, the viewer is marked for a new snapshot, and when its
writer next drains it gets `snapshot` and continues from there. The console's
`v.said` keeps its drop-on-full for OSCs, which is harmless there.

## 4. The console

The frame stays the console's, with these changes.

**Selection.** Besides drag: a double-click selects the word under the
pointer (letters, digits and `_-./:@` run together), a triple-click the
row, Shift-click extends the current selection to the click, as a terminal
does. Bubble Tea's mouse events carry no click count, so the frame keeps the
last press's time and cell: a second press within 400 ms on the same cell is
a double, a third a triple. Copied on release as now.

**Scrollback mode** (`^K [`) gains, after k9s's logs view:

- **follow** (`s` toggles, `G` turns it on): the view sticks to the newest
  output while on; scrolling up turns it off. The bottom row shows the
  state, `follow:on`/`off`, k9s-style, beside `f bare`.
- **filter** (`/`): a line to type a pattern into, `Enter` to apply, `Esc`
  to clear; the view shows only history rows that match, matches in reverse
  video, and the bottom row says `12 of 4,310 lines`. The pattern is a
  case-insensitive regexp; `!` first inverts it. No fuzzy matching.
- **clear** (`C`): the view forgets the history up to now (the emulator's
  scrollback is left alone; a run's `c` copies all of it still).
- `c` copy, `f` bare, `esc`/`q` live: as now.

**The commands menu.** The bottom-left `^K` menu's entries, their labels,
their handlers and when each is offered live in one table in the frame,
which both `hint` and `commanded` read, so a key cannot be offered without a
handler or handled without being offered. The tab's command strip is the
same list, minus the console-only entries, plus the tab-only ones (copy all,
save, search). The table is also where the k9s-style scrollback keys live.

## 5. Routes

- `GET /attach/logs`: `text/plain`, the session's `logs.Log` lines.
- `GET /attach/qr.svg`: the address as an SVG, from `QRLines`.
- `POST /attach/restart`: 204; 409 when not restartable.
- `POST /attach/exit`: 204; ends the run as `x` does.

All four are registered beside `/attach` (`attach.go:824`) and pass the same
gate; a request that fails it gets what `/attach` gives it. `/alive` is
unchanged.

## 6. Testing

Beside their sources.

- **`snapshot`** round-trips: feed the bytes to a fresh `vt` emulator of the
  same size and compare every cell, the cursor, visibility and modes with
  the source, for a corpus: plain text, 16 and 256 and true colours,
  attributes, wide characters, a hyperlink, a wrapped line, history past the
  screen, the alternate screen with content on both, a hidden cursor, mouse
  and bracketed-paste modes on.
- **The tee**: order is preserved across many chunks; a viewer past 1 MiB
  behind gets a snapshot and the bytes after it, never a gap; a parting
  viewer frees its queue.
- **Chrome state**: sent on join; sent again on a title change, a viewer
  joining or leaving, a resize, a motd change; base64 decodes to the fields
  above; `embedded` follows the route.
- **Routes**: each answers as above, and a stranger is refused the way
  `/attach` refuses them.
- **Input**: bytes on STDIN reach the program's input unchanged.
- **The frame**: the action table drives both the hint and the dispatch (a
  table entry with no handler fails a test); double, triple and Shift-click
  selections; `follow` state and its toggles; `/` filter with `!`; `C`.
- **The page** has no test harness today and gets none here: it is verified
  by hand in Chrome on a local tunnel before the PR, and the checks are
  listed in the PR: selection and copy, double and triple click, search,
  copy all and save, each command, the pop-out in multiview, a second tab
  joining mid-vim, a tab reconnecting.
- `docs/reference.md`'s selecting-and-copying section is rewritten for the
  two places.

## 7. Out of scope

- Serving the page from tunnel.pizza (#261).
- The console frame releasing the mouse when the program does not want it:
  a different change to the console's feel, and not asked for.
- Timestamps and "since" presets from k9s: log-specific.
- A page test harness.
