# A native terminal in the tab Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** A browser tab runs xterm on the program's own bytes, with native selection, copying, scrollback and search, HTML chrome around it and a command strip; the console frame gains word/line/extend selection and k9s-style scrollback mode.

**Architecture:** A tab viewer no longer runs the Bubble Tea frame. Its websocket's STDOUT carries chrome state as a private OSC 7770 (base64 JSON), then a styled snapshot of the session's emulator (history, screen, alt screen, cursor, modes, title), then the program's raw stream through a per-viewer bounded tee that re-snapshots instead of dropping bytes. STDIN bytes go straight to the program. The page draws border, bars, motd strips and a command strip as HTML; four new HTTP routes serve logs, a QR SVG, restart and exit. The console keeps the frame, with an action table driving `^K`, click-count selection, and follow/filter/clear in scrollback mode.

**Tech Stack:** Go 1.26, `charmbracelet/x/vt` (emulator), `charmbracelet/ultraviolet` (cells), `charmbracelet/x/ansi`, Bubble Tea v2 (console frame), `k8s.io/cri-streaming` remotecommand (websocket framing), xterm.js 6.0.0 with the fit, webgl, clipboard, search and serialize addons from jsDelivr (pinned, integrity-checked), `rsc.io/qr`.

**Spec:** `docs/superpowers/specs/2026-10-08-native-tab-terminal-design.md` (issue #274)

## Global Constraints

- Chrome OSC: `ESC ] 7770 ; <standard base64 of JSON> ESC \` (ST). JSON fields, exactly: `origin, address, embedded, title, subtitle, banner, host, viewers, cols, rows, motd, restartable, notice`; `motd` is `[{severity, text}]`.
- Tee queue limit: `1 << 20` bytes per tab viewer; a viewer past it gets a fresh snapshot, never a gap.
- The snapshot begins with `ESC c` and is one STDOUT frame.
- Routes: `GET /attach/logs` (text/plain), `GET /attach/qr.svg` (image/svg+xml), `POST /attach/restart` (204, 409 when not restartable), `POST /attach/exit` (204). Registered beside `/attach`, same Origin rule.
- Console double-click window: 400 ms, same cell. Word characters: letters, digits and `_-./:@`.
- Scrollback filter: case-insensitive regexp; a leading `!` inverts. Count line: `N of M lines`.
- The page's CDN scripts are pinned by version with `integrity` and `crossorigin="anonymous" referrerpolicy="no-referrer"`, and `README.md`'s acknowledgements list each pin (readme_test pins it).
- Repository rules: tests beside their source (one `_test.go` per source file); comments state constraints only; branch `feat/native-tab`, PR body `Closes #274`; commits end with `Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>`.
- **Deviations from the spec, decided here:** (1) `vt` exposes no soft-wrap flag, so a snapshot emits every history row with CR LF; a wrapped long line copies as several lines. (2) The frame's `embedded` mode goes: only tabs were embedded, and tabs no longer run the frame. (3) OSC 52 replies typed by a tab's xterm reach the program's stdin like any key, as on a real terminal; the clipboard addon's permission prompt is the guard, and `session.clipboard`'s window stays for the console.

## Review Focus

- A program that changes modes mid-stream (vim turning the mouse on, then off): a tab joining after the off must not see the mouse on. Task 1 pins `TestModesAreRecorded` (set then reset leaves nothing); Task 2 pins the snapshot carrying only set modes.
- A second tab joining while the first is mid-output: both must see the same bytes once, with no chunk duplicated across the join. Task 3 pins `TestTabJoinSplitsTheStreamExactly`.
- A tab whose window is smaller than the pane another viewer settled on: the page must resize xterm down to the pane, not clip. Task 5's manual list pins it (two tabs, one narrowed).
- A run that ends while a tab is attached: the tab's socket must close so the page shows "ended", not hang. Task 3 pins `TestTabEndsWhenTheRunDoes`.
- Keys typed while the command strip is open must not reach the program. Task 5's manual list pins it; the strip is page-side only.

---

## File Structure

| File | Responsibility |
|---|---|
| `v1alpha1/attach/session.go` (+ `session_test.go`) | Private modes recorded; tee publish in `sink.Write`; chrome state; `AttachContainer` builds a tab viewer; negotiate by pane kind |
| `v1alpha1/attach/snapshot.go` (+ `snapshot_test.go`) | `session.snapshot()` |
| `v1alpha1/attach/tab.go` (+ `tab_test.go`) | `tee`, `chrome`, `attachTab`, `rawStdin`, `followTab` |
| `v1alpha1/attach/attach.go` (+ `attach_test.go`) | The four routes |
| `v1alpha1/attach/qr.go` (+ `qr_test.go`) | `QRSVG` |
| `v1alpha1/attach/index.html` | The page: HTML chrome, OSC 7770 handler, command strip, search/serialize |
| `v1alpha1/attach/frame.go` (+ `frame_test.go`) | `embedded` removed; action table; click counting; follow/filter/clear |
| `v1alpha1/attach/selection.go` (+ `selection_test.go`) | `wordAt`, `rowOf` |
| `docs/reference.md`, `README.md` | Docs and pins |

---

### Task 1: Every private mode the program sets is recorded

**Files:**
- Modify: `v1alpha1/attach/session.go` (fields `hidden`, `mouse` → `modes`; `said`'s Mode case; `setMouse` → `setMode`; `mouseWanted`; `resetModes`)
- Test: `v1alpha1/attach/session_test.go`

**Interfaces:**
- Produces: `func (s *session) setMode(mode int, set bool)`, `func (s *session) modesSet() []int` (sorted ascending, private modes currently set, 25 excluded since it is tracked as `hidden`), `func (s *session) mouseWanted() bool` unchanged in meaning.

- [ ] **Step 1: Write the failing test**

Append to `session_test.go`:

```go
// TestModesAreRecorded pins that every private mode the program sets is kept,
// in order, until it clears it or the run resets: what a tab joining later
// has to be told, and what decides whose the wheel is.
func TestModesAreRecorded(t *testing.T) {
	h := newFrameHarness(t)
	s := h.s
	write := func(seq string) {
		t.Helper()
		if _, err := s.scan.Write([]byte(seq)); err != nil {
			t.Fatal(err)
		}
	}
	write("\x1b[?1049h\x1b[?1000h\x1b[?1006h\x1b[?2004h\x1b[?25l")
	if got := s.modesSet(); !slices.Equal(got, []int{1000, 1006, 1049, 2004}) {
		t.Errorf("modesSet() = %v, want 1000 1006 1049 2004 (25 is hidden's)", got)
	}
	if !s.cursorHidden() || !s.mouseWanted() {
		t.Errorf("cursorHidden %v, mouseWanted %v; want both", s.cursorHidden(), s.mouseWanted())
	}
	write("\x1b[?1000l\x1b[?1006l")
	if s.mouseWanted() {
		t.Error("mouseWanted after both mouse modes were cleared")
	}
	if got := s.modesSet(); !slices.Equal(got, []int{1049, 2004}) {
		t.Errorf("modesSet() after clearing = %v, want 1049 2004", got)
	}
	s.resetModes()
	if got := s.modesSet(); len(got) != 0 || s.cursorHidden() {
		t.Errorf("after resetModes: modes %v, hidden %v; want none", got, s.cursorHidden())
	}
}
```

Add `"slices"` to the test file's imports if absent.

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./v1alpha1/attach/ -run TestModesAreRecorded`
Expected: FAIL, `s.modesSet undefined`.

- [ ] **Step 3: Record every mode**

In `session.go`, replace the `hidden`/`mouse` fields and comment with:

```go
	// modeMu guards hidden and modes, and is separate from titleMu for the
	// same reason titleMu is separate from mu: said writes them from the
	// stream goroutine and a frame reads them from its own.
	//
	// hidden is what the program asked for with DECTCEM. modes is every other
	// private mode the program has set and not cleared, by number: the mouse
	// modes decide whose the wheel is, and all of them are what a viewer
	// joining later has to be told so its terminal is in the state the
	// program believes it is.
	modeMu sync.Mutex
	hidden bool
	modes  map[int]bool
```

Replace `setMouse`, `mouseWanted` and `resetModes` with:

```go
// setMode records one private mode being set or cleared. DECTCEM (25) is
// kept as hidden instead.
func (s *session) setMode(mode int, set bool) {
	if mode == 25 {
		s.setCursorHidden(!set)
		return
	}
	s.modeMu.Lock()
	defer s.modeMu.Unlock()
	if !set {
		delete(s.modes, mode)
		return
	}
	if s.modes == nil {
		s.modes = map[int]bool{}
	}
	s.modes[mode] = true
}

// modesSet is every private mode the program has set and not cleared, in
// order, for a snapshot to replay.
func (s *session) modesSet() []int {
	s.modeMu.Lock()
	defer s.modeMu.Unlock()
	out := make([]int, 0, len(s.modes))
	for m := range s.modes {
		out = append(out, m)
	}
	slices.Sort(out)
	return out
}

// mouseWanted reports whether the program has any mouse-reporting mode on,
// which is the frame's cue to hand it the wheel rather than scroll with it.
func (s *session) mouseWanted() bool {
	s.modeMu.Lock()
	defer s.modeMu.Unlock()
	for _, m := range []int{9, 1000, 1001, 1002, 1003} {
		if s.modes[m] {
			return true
		}
	}
	return false
}

// resetModes forgets what the program asked for, for a program that is gone.
// The next one starts from a terminal's defaults, the way the emulator does
// on the RIS revive writes.
func (s *session) resetModes() {
	s.modeMu.Lock()
	s.hidden = false
	s.modes = nil
	s.modeMu.Unlock()
}
```

In `said`, the `case Mode:` body becomes:

```go
	case Mode:
		// The CSI is already on the screen. Recorded for a viewer joining
		// later, and for the frame: whether a cursor is wanted, and whether
		// the mouse is — which decides whose the wheel is.
		s.setMode(seq.Cmd, seq.Set)
```

Add `"slices"` to `session.go`'s imports.

- [ ] **Step 4: Run the package**

Run: `go test ./v1alpha1/attach/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add v1alpha1/attach/session.go v1alpha1/attach/session_test.go
git commit -m "feat(attach): record every private mode the program sets (#274)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 2: The snapshot

**Files:**
- Create: `v1alpha1/attach/snapshot.go`, `v1alpha1/attach/snapshot_test.go`

**Interfaces:**
- Consumes: `s.modesSet()`, `s.cursorHidden()`, `s.titles()` (Task 1 and existing)
- Produces: `func (s *session) snapshot() []byte`

- [ ] **Step 1: Write the failing test**

`snapshot_test.go`:

```go
package attach

import (
	"bytes"
	"fmt"
	"strings"
	"testing"

	"github.com/charmbracelet/x/vt"
)

// TestSnapshotRoundTrips pins the snapshot against a fresh emulator: fed the
// bytes, it holds the same screen, history, cursor and alternate screen as
// the session's. Every kind of thing a program draws is in the corpus.
func TestSnapshotRoundTrips(t *testing.T) {
	var long strings.Builder
	for i := 1; i <= 30; i++ {
		fmt.Fprintf(&long, "line %02d\r\n", i)
	}
	for _, tc := range []struct {
		name, script string
	}{
		{"plain text", "hello\r\nworld"},
		{"sixteen colours and attributes", "\x1b[31;1mred bold\x1b[m \x1b[4;3munder italic\x1b[m \x1b[7mrev\x1b[m \x1b[9mstrike\x1b[m \x1b[2mfaint\x1b[m"},
		{"256 and true colours", "\x1b[38;5;208morange\x1b[48;2;1;2;3m on blue\x1b[m"},
		{"wide characters", "日本語 x"},
		{"a hyperlink", "\x1b]8;;https://example.com\x1b\\here\x1b]8;;\x1b\\ there"},
		{"history past the screen", long.String()},
		{"the alternate screen", "main text\r\n\x1b[?1049h\x1b[2;3Halt text"},
		{"a moved cursor", "abc\x1b[3;5H"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newFrameHarness(t)
			s := h.s
			s.screen.Lock()
			s.em.Resize(20, 5)
			s.screen.Unlock()
			if _, err := s.scan.Write([]byte(tc.script)); err != nil {
				t.Fatal(err)
			}
			snap := s.snapshot()
			if !bytes.HasPrefix(snap, []byte("\x1bc")) {
				t.Fatalf("a snapshot starts with a reset; got %q", snap[:min(8, len(snap))])
			}
			fresh := vt.NewSafeEmulator(20, 5)
			if _, err := fresh.Write(snap); err != nil {
				t.Fatal(err)
			}
			if fresh.IsAltScreen() != s.em.IsAltScreen() {
				t.Errorf("alt screen %v, want %v", fresh.IsAltScreen(), s.em.IsAltScreen())
			}
			if got, want := fresh.ScrollbackLen(), s.em.ScrollbackLen(); got != want {
				t.Errorf("history %d lines, want %d", got, want)
			}
			for y := range s.em.ScrollbackLen() {
				for x := range 20 {
					if !sameCell(fresh.ScrollbackCellAt(x, y), s.em.ScrollbackCellAt(x, y)) {
						t.Errorf("history cell (%d,%d) = %v, want %v", x, y, fresh.ScrollbackCellAt(x, y), s.em.ScrollbackCellAt(x, y))
					}
				}
			}
			for y := range 5 {
				for x := range 20 {
					if !sameCell(fresh.CellAt(x, y), s.em.CellAt(x, y)) {
						t.Errorf("cell (%d,%d) = %v, want %v", x, y, fresh.CellAt(x, y), s.em.CellAt(x, y))
					}
				}
			}
			if got, want := fresh.CursorPosition(), s.em.CursorPosition(); got != want {
				t.Errorf("cursor %v, want %v", got, want)
			}
		})
	}
}

// TestSnapshotCarriesTheModes pins what a fresh emulator cannot be asked
// about: the modes the program set, its hidden cursor, and its title go in
// the bytes.
func TestSnapshotCarriesTheModes(t *testing.T) {
	h := newFrameHarness(t)
	s := h.s
	if _, err := s.scan.Write([]byte("\x1b]2;my title\x07\x1b[?1000h\x1b[?1006h\x1b[?2004h\x1b[?25l")); err != nil {
		t.Fatal(err)
	}
	snap := string(s.snapshot())
	for _, want := range []string{"\x1b[?1000h", "\x1b[?1006h", "\x1b[?2004h", "\x1b[?25l", "\x1b]2;my title\x07"} {
		if !strings.Contains(snap, want) {
			t.Errorf("snapshot lacks %q", want)
		}
	}
	if _, err := s.scan.Write([]byte("\x1b[?1000l\x1b[?25h")); err != nil {
		t.Fatal(err)
	}
	snap = string(s.snapshot())
	if strings.Contains(snap, "\x1b[?1000h") || strings.Contains(snap, "\x1b[?25l") {
		t.Error("a cleared mode, or a shown cursor, is still in the snapshot")
	}
}

// sameCell compares what a viewer would see: content, width, style and link.
func sameCell(a, b *uv.Cell) bool {
	blank := func(c *uv.Cell) bool {
		return c == nil || ((c.Content == "" || c.Content == " ") && c.Style.IsZero() && c.Link.URL == "")
	}
	if blank(a) && blank(b) {
		return true
	}
	if a == nil || b == nil {
		return false
	}
	return a.Content == b.Content && a.Width == b.Width && a.Style.Equal(&b.Style) && a.Link.URL == b.Link.URL
}
```

Add `uv "github.com/charmbracelet/ultraviolet"` to the imports.

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./v1alpha1/attach/ -run TestSnapshot`
Expected: FAIL, `s.snapshot undefined`.

- [ ] **Step 3: Write `snapshot.go`**

```go
package attach

import (
	"bytes"
	"fmt"

	uv "github.com/charmbracelet/ultraviolet"
	"github.com/charmbracelet/x/ansi"
)

// snapshot is the terminal as it stands, as bytes that leave a fresh xterm in
// the same state: a reset, the title, the main screen's history and rows,
// the alternate screen if the program is on it, the cursor, and every
// private mode the program has set. Taken under the screen lock, so it is
// consistent with the stream up to the chunk before it.
//
// History rows end in CR LF, so a line the emulator wrapped is several rows
// here: vt keeps no wrap flag to join them by.
func (s *session) snapshot() []byte {
	s.screen.RLock()
	defer s.screen.RUnlock()

	var b bytes.Buffer
	b.WriteString("\x1bc")
	if title, _ := s.titles(); title != "" {
		b.WriteString(ansi.SetWindowTitle(title))
	}

	w, h := s.em.Width(), s.em.Height()
	var st styled
	kept := s.em.ScrollbackLen()
	for y := range kept {
		st.row(&b, w, func(x int) *uv.Cell { return s.em.ScrollbackCellAt(x, y) })
		b.WriteString("\r\n")
	}
	// The main screen's rows follow the history as more lines, so what is on
	// screen is the last h lines written and the history is above them.
	main := uv.NewScreenBuffer(w, h)
	if s.em.IsAltScreen() {
		// drawPaneLocked reads the current screen; the main one is behind
		// the alternate. vt keeps no accessor for the inactive screen, so
		// the main screen's rows are blank under the alternate one.
	} else {
		s.drawPaneLocked(main, main.Bounds())
	}
	for y := range h {
		st.row(&b, w, func(x int) *uv.Cell { return main.CellAt(x, y) })
		if y < h-1 {
			b.WriteString("\r\n")
		}
	}
	if s.em.IsAltScreen() {
		st.reset(&b)
		b.WriteString(ansi.SetModeAltScreenSaveCursor)
		alt := uv.NewScreenBuffer(w, h)
		s.drawPaneLocked(alt, alt.Bounds())
		for y := range h {
			b.WriteString(ansi.CursorPosition(1, y+1))
			st.row(&b, w, func(x int) *uv.Cell { return alt.CellAt(x, y) })
		}
	}
	st.reset(&b)

	pos := s.em.CursorPosition()
	b.WriteString(ansi.CursorPosition(pos.X+1, pos.Y+1))
	for _, m := range s.modesSet() {
		if m == 1049 {
			continue // set above, with its screen
		}
		fmt.Fprintf(&b, "\x1b[?%dh", m)
	}
	if s.cursorHidden() {
		b.WriteString(ansi.ResetModeTextCursorEnable)
	}
	return b.Bytes()
}

// styled writes cells as text with the SGR and hyperlink transitions between
// them, remembering what the terminal was last told.
type styled struct {
	cur  uv.Style
	link uv.Link
}

// row writes one row of w cells: a wide character once, its tail skipped,
// and trailing blank cells with no style left off.
func (st *styled) row(b *bytes.Buffer, w int, at func(x int) *uv.Cell) {
	last := -1
	for x := 0; x < w; x++ {
		if c := at(x); c != nil && !(c.Width == 0 && c.Content == "") &&
			!((c.Content == "" || c.Content == " ") && c.Style.IsZero() && c.Link.URL == "") {
			last = x
		}
	}
	for x := 0; x <= last; {
		c := at(x)
		if c == nil {
			st.set(b, uv.Style{}, uv.Link{})
			b.WriteByte(' ')
			x++
			continue
		}
		if c.Width == 0 && c.Content == "" {
			x++ // a wide character's tail
			continue
		}
		st.set(b, c.Style, c.Link)
		if c.Content == "" {
			b.WriteByte(' ')
		} else {
			b.WriteString(c.Content)
		}
		x += max(1, c.Width)
	}
}

func (st *styled) set(b *bytes.Buffer, style uv.Style, link uv.Link) {
	if !style.Equal(&st.cur) {
		b.WriteString(style.Diff(&st.cur))
		st.cur = style
	}
	if link != st.link {
		if link.URL == "" {
			b.WriteString(ansi.ResetHyperlink())
		} else {
			b.WriteString(ansi.SetHyperlink(link.URL))
		}
		st.link = link
	}
}

func (st *styled) reset(b *bytes.Buffer) { st.set(b, uv.Style{}, uv.Link{}) }
```

- [ ] **Step 4: Run it**

Run: `go test ./v1alpha1/attach/ -run TestSnapshot -v`
Expected: PASS for every row. If "the alternate screen" fails on the main screen's cells, that is the deviation the comment states (vt has no accessor for the inactive screen): change that row's script so the main screen is empty before `?1049h` is not an option, because the test is checking the alternate screen's own cells; compare only when `!IsAltScreen` for the main rows by wrapping the main-screen loop in `if !s.em.IsAltScreen()`. Ledger it as a ruling if so.

- [ ] **Step 5: Commit**

```bash
git add v1alpha1/attach/snapshot.go v1alpha1/attach/snapshot_test.go
git commit -m "feat(attach): a snapshot of the terminal for a viewer joining (#274)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 3: Tab viewers: tee, chrome, raw input

**Files:**
- Create: `v1alpha1/attach/tab.go`, `v1alpha1/attach/tab_test.go`
- Modify: `v1alpha1/attach/session.go` (`viewer`, `sink.Write`, `join`/`part`/`announce`/`setName`/`motdChanged`/`resizeViewer`, `negotiate`, `AttachContainer`), `v1alpha1/attach/frame.go` (remove `embedded`), `v1alpha1/attach/frame_test.go` (remove the tab-only tests)

**Interfaces:**
- Consumes: `s.snapshot()` (Task 2), `s.modesSet()` (Task 1)
- Produces:
  - `type tee struct` with `offer(p []byte)`, `take() (chunks [][]byte, stale bool)`, `wait chan struct{}`; `const teeLimit = 1 << 20`
  - `type chrome struct` (JSON) and `func (s *session) chromeBytes(embedded bool) []byte`
  - `func (s *session) chromeChanged()`
  - `viewer.tab *tee`, `viewer.embedded bool`
  - `func (s *session) attachTab(ctx context.Context, cancel context.CancelFunc, v *viewer, in io.Reader, out io.Writer, resize <-chan remotecommand.TerminalSize)`

- [ ] **Step 1: Write the failing tests**

`tab_test.go`:

```go
package attach

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// tabOut is a tab's STDOUT: what the session wrote, and a way to hold the
// writer up so the tee fills.
type tabOut struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	hold chan struct{} // nil: never held
}

func (o *tabOut) Write(p []byte) (int, error) {
	if o.hold != nil {
		<-o.hold
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.Write(p)
}
func (o *tabOut) Close() error { return nil }
func (o *tabOut) String() string {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.buf.String()
}

// openTab attaches one tab viewer to h's session and returns its output, its
// stdin writer, and a cancel that is the socket closing.
func openTab(t *testing.T, h *harness, out *tabOut) (typed *io.PipeWriter, cancel func()) {
	t.Helper()
	in, typed := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	resize := make(chan remotecommand.TerminalSize)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.s.AttachContainer(ctx, "", "", "", in, out, nil, true, resize)
	}()
	t.Cleanup(func() {
		cancel()
		_ = typed.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Error("the tab never ended")
		}
	})
	return typed, cancel
}

// await waits for want in out.
func await(t *testing.T, out *tabOut, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if strings.Contains(out.String(), want) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("waited for %q; the tab got %q", want, out.String())
}

// chromeOf decodes the last chrome message in s.
func chromeOf(t *testing.T, s string) chrome {
	t.Helper()
	i := strings.LastIndex(s, "\x1b]7770;")
	if i < 0 {
		t.Fatalf("no chrome message in %q", s)
	}
	rest := s[i+len("\x1b]7770;"):]
	end := strings.Index(rest, "\x1b\\")
	raw, err := base64.StdEncoding.DecodeString(rest[:end])
	if err != nil {
		t.Fatal(err)
	}
	var c chrome
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestTabGetsChromeSnapshotThenStream pins the order a tab's socket carries:
// chrome state, a snapshot of what is on screen already, then the bytes
// the program writes from then on, untouched.
func TestTabGetsChromeSnapshotThenStream(t *testing.T) {
	h := newFrameHarness(t)
	if _, err := (&sink{s: h.s}).Write([]byte("before\r\n")); err != nil {
		t.Fatal(err)
	}
	out := &tabOut{}
	openTab(t, h, out)
	await(t, out, "before")
	got := out.String()
	c := strings.Index(got, "\x1b]7770;")
	r := strings.Index(got, "\x1bc")
	if c != 0 || r < c {
		t.Fatalf("chrome at %d, reset at %d; want the chrome first then the snapshot: %q", c, r, got)
	}
	ch := chromeOf(t, got)
	if ch.Origin == "" || ch.Viewers != 2 || ch.Cols == 0 || ch.Rows == 0 {
		t.Errorf("chrome = %+v; want the origin, 2 viewers (the harness's frame and this tab), a size", ch)
	}
	if _, err := (&sink{s: h.s}).Write([]byte("\x1b]52;c;aGk=\x07after \x1b[31mred\x1b[m\r\n")); err != nil {
		t.Fatal(err)
	}
	await(t, out, "after \x1b[31mred\x1b[m")
	if !strings.Contains(out.String()[r:], "\x1b]52;c;aGk=\x07") {
		t.Error("an OSC the program wrote did not reach the tab as it was")
	}
}

// TestTabJoinSplitsTheStreamExactly pins that a tab joining between two
// chunks sees the first in its snapshot and the second in its stream, and
// neither twice.
func TestTabJoinSplitsTheStreamExactly(t *testing.T) {
	h := newFrameHarness(t)
	w := &sink{s: h.s}
	_, _ = w.Write([]byte("one\r\n"))
	out := &tabOut{}
	openTab(t, h, out)
	await(t, out, "one")
	_, _ = w.Write([]byte("two\r\n"))
	await(t, out, "two")
	got := out.String()
	if strings.Count(got, "one") != 1 || strings.Count(got, "two") != 1 {
		t.Errorf("one ×%d, two ×%d in %q; want each once", strings.Count(got, "one"), strings.Count(got, "two"), got)
	}
	if strings.Index(got, "one") > strings.Index(got, "two") {
		t.Error("two came before one")
	}
}

// TestTabFallingBehindIsResnapshotted pins the tee's limit: a tab that has not
// read a mebibyte is not fed stale bytes, and does not miss any either — it
// gets a fresh snapshot and everything after it.
func TestTabFallingBehindIsResnapshotted(t *testing.T) {
	h := newFrameHarness(t)
	out := &tabOut{hold: make(chan struct{})}
	openTab(t, h, out)
	time.Sleep(50 * time.Millisecond) // the first writes are parked on hold
	w := &sink{s: h.s}
	chunk := []byte(strings.Repeat("x", 64<<10))
	for range 20 { // 1.25 MiB, past the limit
		_, _ = w.Write(chunk)
	}
	_, _ = w.Write([]byte("LAST\r\n"))
	close(out.hold)
	await(t, out, "LAST")
	got := out.String()
	if strings.Count(got, "\x1bc") < 2 {
		t.Errorf("a tab past the limit should have been snapshotted again; resets: %d", strings.Count(got, "\x1bc"))
	}
	if strings.LastIndex(got, "\x1bc") > strings.LastIndex(got, "LAST") {
		t.Error("the last chunk came before the fresh snapshot")
	}
}

// TestTabTypesStraightToTheProgram pins a tab's STDIN reaching the program
// as it was typed: xterm encoded it, and nothing here decodes it again.
func TestTabTypesStraightToTheProgram(t *testing.T) {
	h := newFrameHarness(t)
	out := &tabOut{}
	typed, _ := openTab(t, h, out)
	await(t, out, "\x1bc")
	_, _ = typed.Write([]byte("ls\r\x1b[A"))
	select {
	case got := <-h.typed:
		if got != "ls\r\x1b[A" {
			t.Errorf("the program read %q, want the bytes as typed", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("nothing reached the program")
	}
}

// TestTabEndsWhenTheRunDoes pins a tab's socket ending with the run, so the
// page can say so, and staying through a restart.
func TestTabEndsWhenTheRunDoes(t *testing.T) {
	h := newFrameHarness(t)
	out := &tabOut{}
	in, _ := io.Pipe()
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	resize := make(chan remotecommand.TerminalSize)
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = h.s.AttachContainer(ctx, "", "", "", in, out, nil, true, resize)
	}()
	await(t, out, "\x1bc")
	close(h.s.done)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the tab outlived the run")
	}
}

// TestTabChromeFollowsTheSession pins the chrome being sent again when
// something it says changes: the title, the address, a viewer leaving.
func TestTabChromeFollowsTheSession(t *testing.T) {
	h := newFrameHarness(t)
	out := &tabOut{}
	openTab(t, h, out)
	await(t, out, "\x1bc")
	if _, err := h.s.scan.Write([]byte("\x1b]2;named\x07")); err != nil {
		t.Fatal(err)
	}
	await(t, out, base64.StdEncoding.EncodeToString([]byte(`"title":"named"`))[:12])
	h.s.announce("https://x.tunneled.test/")
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) && chromeOf(t, out.String()).Address == "" {
		time.Sleep(10 * time.Millisecond)
	}
	if c := chromeOf(t, out.String()); c.Address != "https://x.tunneled.test/" || c.Title != "named" {
		t.Errorf("chrome = %+v; want the address and the title", c)
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./v1alpha1/attach/ -run 'TestTab'`
Expected: build FAIL, `undefined: chrome`.

- [ ] **Step 3: Write `tab.go`**

```go
package attach

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"sync"

	"k8s.io/cri-streaming/pkg/streaming/remotecommand"
)

// teeLimit is how many bytes a tab may be behind before it is given a fresh
// snapshot instead of the bytes it missed.
const teeLimit = 1 << 20

// tee is the program's stream queued for one tab, bounded. A tab that falls
// past the limit has its queue dropped and is marked stale: the writer then
// sends a snapshot and continues from there, so nothing is ever lost quietly.
type tee struct {
	mu     sync.Mutex
	queue  [][]byte
	bytes  int
	stale  bool
	wake   chan struct{}
	closed bool
}

func newTee() *tee { return &tee{wake: make(chan struct{}, 1)} }

// offer queues p, copied, for the writer; past the limit the queue goes and
// the tab is stale instead.
func (t *tee) offer(p []byte) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return
	}
	if t.bytes+len(p) > teeLimit {
		t.queue, t.bytes, t.stale = nil, 0, true
	} else {
		t.queue = append(t.queue, append([]byte(nil), p...))
		t.bytes += len(p)
	}
	t.mu.Unlock()
	select {
	case t.wake <- struct{}{}:
	default:
	}
}

// take hands the writer everything queued, and whether a snapshot is owed
// first.
func (t *tee) take() ([][]byte, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	chunks, stale := t.queue, t.stale
	t.queue, t.bytes, t.stale = nil, 0, false
	return chunks, stale
}

func (t *tee) close() {
	t.mu.Lock()
	t.closed = true
	t.queue, t.bytes = nil, 0
	t.mu.Unlock()
}

// chrome is what the page draws around the terminal, as the session knows
// it. Sent as OSC 7770 with the JSON in base64, so the payload has no
// control bytes for the terminal to act on.
type chrome struct {
	Origin      string        `json:"origin"`
	Address     string        `json:"address"`
	Embedded    bool          `json:"embedded"`
	Title       string        `json:"title"`
	Subtitle    string        `json:"subtitle"`
	Banner      string        `json:"banner"`
	Host        string        `json:"host"`
	Viewers     int           `json:"viewers"`
	Cols        int           `json:"cols"`
	Rows        int           `json:"rows"`
	Motd        []chromeMotd  `json:"motd"`
	Restartable bool          `json:"restartable"`
	Notice      string        `json:"notice"`
}

type chromeMotd struct {
	Severity string `json:"severity"`
	Text     string `json:"text"`
}

// chromeBytes is the chrome message for a viewer, embedded or not.
func (s *session) chromeBytes(embedded bool) []byte {
	title, subtitle := s.titles()
	w, h := s.paneSize()
	c := chrome{
		Origin:      s.Origin(),
		Address:     s.announced(),
		Embedded:    embedded,
		Title:       title,
		Subtitle:    subtitle,
		Banner:      s.banner,
		Host:        host(),
		Viewers:     s.count(),
		Cols:        w,
		Rows:        h,
		Motd:        []chromeMotd{},
		Restartable: s.restartable(),
		Notice:      s.notice,
	}
	if s.motd != nil && !embedded {
		for _, m := range s.motd.Messages() {
			c.Motd = append(c.Motd, chromeMotd{Severity: m.Severity, Text: m.Text})
		}
	}
	raw, _ := json.Marshal(c)
	return []byte("\x1b]7770;" + base64.StdEncoding.EncodeToString(raw) + "\x1b\\")
}

// chromeChanged sends every tab its chrome again. Off teeMu: it is called
// from said, which runs inside sink.Write's hold of it.
func (s *session) chromeChanged() {
	s.mu.Lock()
	tabs := make([]*viewer, 0, len(s.viewers))
	for v := range s.viewers {
		if v.tab != nil {
			tabs = append(tabs, v)
		}
	}
	s.mu.Unlock()
	for _, v := range tabs {
		v.tab.offer(s.chromeBytes(v.embedded))
	}
}

// tees hands a chunk of the program's stream to every tab. The caller holds
// teeMu, which is what keeps a join's snapshot on a chunk boundary.
func (s *session) tees(p []byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for v := range s.viewers {
		if v.tab != nil {
			v.tab.offer(p)
		}
	}
}

// rawStdin writes a tab's keystrokes to the program as they came: xterm
// encoded them for the modes it saw the program set.
type rawStdin struct{ s *session }

func (w rawStdin) Write(p []byte) (int, error) {
	w.s.mu.Lock()
	stdin := w.s.stdin
	w.s.mu.Unlock()
	if stdin == nil {
		return len(p), nil
	}
	return stdin.Write(p)
}

// attachTab serves one tab: the chrome, a snapshot taken on a chunk
// boundary, then the stream from that boundary on. It returns when the
// socket is gone or the run ends without a restart.
func (s *session) attachTab(ctx context.Context, cancel context.CancelFunc, v *viewer, in io.Reader, out io.Writer, resize <-chan remotecommand.TerminalSize) {
	defer cancel()

	s.teeMu.Lock()
	s.mu.Lock()
	s.viewers[v] = struct{}{}
	s.mu.Unlock()
	first := append(s.chromeBytes(v.embedded), s.snapshot()...)
	s.teeMu.Unlock()
	s.wakeAll()
	s.chromeChanged()
	defer s.part(v)
	defer v.tab.close()

	if _, err := out.Write(first); err != nil {
		return
	}
	if in != nil {
		go func() { _, _ = io.Copy(rawStdin{s}, in) }()
	}
	go s.followTab(ctx, cancel, v, resize)

	for {
		select {
		case <-v.tab.wake:
		case <-ctx.Done():
			return
		}
		chunks, stale := v.tab.take()
		if stale {
			s.teeMu.Lock()
			chunks, _ = v.tab.take()
			snap := append(s.chromeBytes(v.embedded), s.snapshot()...)
			s.teeMu.Unlock()
			if _, err := out.Write(snap); err != nil {
				return
			}
		}
		for _, c := range chunks {
			if _, err := out.Write(c); err != nil {
				return
			}
		}
	}
}

// followTab is follow for a tab: a size is the pane the tab can show and
// goes straight to the negotiation, and the run ending ends the tab unless
// a restart replaces it.
func (s *session) followTab(ctx context.Context, cancel context.CancelFunc, v *viewer, resize <-chan remotecommand.TerminalSize) {
	defer cancel()
	for {
		done := s.ended()
		select {
		case size, ok := <-resize:
			if !ok {
				return
			}
			if size.Width > 0 && size.Height > 0 {
				s.resizeViewer(v, int(size.Width), int(size.Height))
				s.chromeChanged()
			}
		case <-ctx.Done():
			return
		case <-done:
			if over := s.restartInFlight(); over != nil {
				<-over
			}
			if s.ended() != done {
				continue
			}
			return
		}
	}
}
```

- [ ] **Step 4: Wire the session**

In `session.go`:

1. `viewer` gains two fields after `said`:

```go
	// tab is the program's stream queued for a browser tab, nil for a
	// console viewer, whose frame draws from the emulator instead. embedded
	// is the tab being a multiview tile.
	tab      *tee
	embedded bool
```

2. `session` gains, beside `screen`:

```go
	// teeMu holds the stream still while a tab joins: sink.Write takes it
	// around a chunk and its hand-off to the tabs, so a snapshot taken under
	// it sits exactly between two chunks.
	teeMu sync.Mutex
	// notice is the page's line about what the target cannot do; see Serve.
	notice string
```

3. `sink.Write` becomes:

```go
func (w *sink) Write(p []byte) (int, error) {
	s := w.s
	// Through the scanner, which writes the screen bytes to the emulator and
	// hands every OSC and private mode to said, and to every tab as it is;
	// under teeMu so a tab joining gets a snapshot on a chunk boundary. A
	// frame woken before the emulator had the bytes would render the screen
	// as it was, so the wake is after.
	s.teeMu.Lock()
	_, _ = s.scan.Write(p)
	s.tees(p)
	s.teeMu.Unlock()
	s.wakeAll()
	return len(p), nil
}
```

4. `negotiate`'s loop takes each viewer's size as a pane when it is a tab:

```go
	var w, h uint16
	banner := uint16(s.bannerRows())
	for v := range s.viewers {
		if v.size.Width == 0 || v.size.Height == 0 {
			continue
		}
		// A tab says how big a pane it can show; a console says its window,
		// which the frame's border and the banner take rows and columns of.
		size := v.size
		if v.tab != nil {
			size = remotecommand.TerminalSize{Width: v.size.Width + chromeWidth, Height: v.size.Height + chromeHeight + banner}
		}
		if w == 0 || size.Width < w {
			w = size.Width
		}
		if h == 0 || size.Height < h {
			h = size.Height
		}
	}
```

(the rest unchanged: `s.size` stays a window).

5. `join` and `part` and `announce` and `motdChanged` each call `s.chromeChanged()` after their `wakeAll()`; `setName` calls `s.chromeChanged()` after setting.

6. `AttachContainer`'s body from `width, height := s.window()` on becomes:

```go
	embedded, _ := ctx.Value(embeddedKey{}).(bool)
	v := &viewer{wake: make(chan struct{}, 1), said: make(chan []byte, 64), tab: newTee(), embedded: embedded}
	s.attachTab(ctx, cancel, v, in, out, resize)
	return nil
```

and its comment is rewritten: "A tab is served the program's own stream: the chrome, a snapshot of the screen as it stands, then every byte from there on, with its keystrokes written to the program as xterm encoded them. The frame is the console's; see viewLocally."

7. `Serve` in `attach.go` stores the notice on the session: move the `notice` computation above `newSession` and set `s.session.notice = notice` right after `newSession`; the page handler reads `s.session.notice`.

8. `motd.Motd` needs `Messages()`: check `v1alpha1/motd` for an accessor returning severity and text; if only `Lines(width)` exists, add to the `Motd` interface and its impl `Messages() []Message` where `Message{Severity, Text string}` (Text: the markdown with its `> [!x]` line removed and blockquote markers stripped). Add a test in `motd/render_test.go`: a message `> [!warning]\n> Be careful` gives `{"warning", "Be careful"}`.

9. `frame.go`: delete the `embedded` field and every branch on it (`box`, `barRows`, `drawBanner`, `where`); `frame_test.go`: delete `TestAnEmbeddedCornerIsAPopout` and `TestATabHasNoScrollbackMode`; `TestEveryFrameAsksForTheMouse` stays. `selection.go`'s comment "The browser tab never sees one of these" becomes "A browser tab runs no frame: xterm selects there."

- [ ] **Step 5: Run the package**

Run: `go test ./v1alpha1/attach/... ./v1alpha1/motd/...`
Expected: PASS, including the six `TestTab*`. A test that hangs points at teeMu being held across `chromeChanged` (it must not be) or at `part` racing `close`.

- [ ] **Step 6: Commit**

```bash
git add v1alpha1/attach v1alpha1/motd
git commit -m "feat(attach): a tab is served the program's own stream, with a snapshot on join (#274)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 4: Routes: logs, qr.svg, restart, exit

**Files:**
- Modify: `v1alpha1/attach/attach.go`, `v1alpha1/attach/qr.go`
- Test: `v1alpha1/attach/attach_test.go`, `v1alpha1/attach/qr_test.go`

**Interfaces:**
- Produces: `func QRSVG(text string) ([]byte, error)`; routes as in Global Constraints.

- [ ] **Step 1: Write the failing tests**

Append to `qr_test.go`:

```go
// TestQRSVGIsACode pins the SVG: one rect per dark module on a light field,
// square, with the quiet zone.
func TestQRSVGIsACode(t *testing.T) {
	svg, err := QRSVG("https://x.tunneled.test/")
	if err != nil {
		t.Fatal(err)
	}
	s := string(svg)
	if !strings.HasPrefix(s, "<svg") || !strings.Contains(s, "<rect") || !strings.Contains(s, `viewBox="0 0 `) {
		t.Errorf("not an SVG of rects: %q", s[:min(120, len(s))])
	}
	if _, err := QRSVG(strings.Repeat("x", 5000)); err == nil {
		t.Error("a text too long to encode gave no error")
	}
}
```

Append to `attach_test.go`:

```go
// TestTheCommandRoutes pins the page's commands: logs and the code to read,
// restart and exit to do, each behind the socket's Origin rule.
func TestTheCommandRoutes(t *testing.T) {
	target := newRerunTarget(false)
	target.holdFrom = 1
	t.Cleanup(target.release)
	s := serveFake(t, target)
	target.awaitRun(t, 1)
	base := s.URL().String()

	get := func(path string) (*http.Response, string) {
		t.Helper()
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		return resp, string(body)
	}
	post := func(path string, origin string) int {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, base+path, nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		_ = resp.Body.Close()
		return resp.StatusCode
	}

	if resp, body := get("/attach/logs"); resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "text/plain; charset=utf-8" || !strings.Contains(body, testLogs{}.Lines()[0]) {
		t.Errorf("GET /attach/logs = %d %q %q", resp.StatusCode, resp.Header.Get("Content-Type"), body)
	}
	s.session.announce("https://x.tunneled.test/")
	if resp, body := get("/attach/qr.svg"); resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/svg+xml" || !strings.HasPrefix(body, "<svg") {
		t.Errorf("GET /attach/qr.svg = %d %q", resp.StatusCode, resp.Header.Get("Content-Type"))
	}
	if code := post("/attach/restart", "http://elsewhere.test"); code != http.StatusForbidden {
		t.Errorf("a cross-origin POST /attach/restart = %d, want 403", code)
	}
	if code := post("/attach/restart", ""); code != http.StatusConflict {
		t.Errorf("POST /attach/restart on a target that cannot restart = %d, want 409", code)
	}
	if code := post("/attach/exit", ""); code != http.StatusNoContent {
		t.Errorf("POST /attach/exit = %d, want 204", code)
	}
	select {
	case <-s.Done():
	case <-time.After(5 * time.Second):
		t.Error("exit did not ask the run to end")
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./v1alpha1/attach/ -run 'TestQRSVGIsACode|TestTheCommandRoutes'`
Expected: FAIL, `undefined: QRSVG`.

- [ ] **Step 3: Write `QRSVG` and the routes**

Append to `qr.go`:

```go
// QRSVG is text as a code in SVG: a rect per dark module on a light field,
// the quiet zone around it, sized in modules so the page scales it.
func QRSVG(text string) ([]byte, error) {
	code, err := qr.Encode(text, qr.L)
	if err != nil {
		return nil, err
	}
	n := code.Size + 2*quiet
	var b strings.Builder
	fmt.Fprintf(&b, `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 %d %d" shape-rendering="crispEdges">`, n, n)
	fmt.Fprintf(&b, `<rect width="%d" height="%d" fill="#fff"/>`, n, n)
	for y := range code.Size {
		for x := range code.Size {
			if code.Black(x, y) {
				fmt.Fprintf(&b, `<rect x="%d" y="%d" width="1" height="1"/>`, x+quiet, y+quiet)
			}
		}
	}
	b.WriteString(`</svg>`)
	return []byte(b.String()), nil
}
```

(add `"fmt"` to `qr.go`'s imports.)

In `attach.go`, lift the Origin check into a helper above `attach`:

```go
	// sameOrigin is the socket's rule for a request that acts on the
	// session: a browser's Origin must be this host; none means a non-browser
	// client.
	sameOrigin := func(w http.ResponseWriter, r *http.Request) bool {
		if o := r.Header.Get("Origin"); o != "" {
			if u, err := url.Parse(o); err != nil || u.Host != r.Host {
				http.Error(w, "attach: cross-origin request refused", http.StatusForbidden)
				return false
			}
		}
		return true
	}
```

make `attach` call `if !sameOrigin(w, r) { return }` in place of its inline check, and register after `/attach/embedded`:

```go
	// The page's commands: what the frame's ^K does on a console, as routes
	// a tab's strip can call.
	mux.HandleFunc("GET /attach/logs", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, strings.Join(s.session.logLines(), "\n"))
	})
	mux.HandleFunc("GET /attach/qr.svg", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(w, r) {
			return
		}
		addr := s.session.announced()
		if addr == "" {
			http.Error(w, "no address yet", http.StatusNotFound)
			return
		}
		svg, err := QRSVG(addr)
		if err != nil {
			http.Error(w, err.Error(), http.StatusUnprocessableEntity)
			return
		}
		w.Header().Set("Content-Type", "image/svg+xml")
		_, _ = w.Write(svg)
	})
	mux.HandleFunc("POST /attach/restart", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(w, r) {
			return
		}
		if !s.session.restartable() {
			http.Error(w, "this program cannot be started over", http.StatusConflict)
			return
		}
		go s.session.restart()
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /attach/exit", func(w http.ResponseWriter, r *http.Request) {
		if !sameOrigin(w, r) {
			return
		}
		s.session.endRun()
		w.WriteHeader(http.StatusNoContent)
	})
```

- [ ] **Step 4: Run them**

Run: `go test ./v1alpha1/attach/ -run 'TestQRSVGIsACode|TestTheCommandRoutes|TestAlive'`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add v1alpha1/attach/attach.go v1alpha1/attach/attach_test.go v1alpha1/attach/qr.go v1alpha1/attach/qr_test.go
git commit -m "feat(attach): routes for the page's commands: logs, qr, restart, exit (#274)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 5: The page

**Files:**
- Modify: `v1alpha1/attach/index.html`, `README.md` (acknowledgements pins)
- Test: `go test ./ -run Acknowledgements` (pins), then the manual list below

**Interfaces:**
- Consumes: OSC 7770 chrome (Task 3), the routes (Task 4).

- [ ] **Step 1: Pin the two addons**

Run:
```bash
for p in @xterm/addon-search @xterm/addon-serialize; do v=$(npm view $p version); u="https://cdn.jsdelivr.net/npm/$p@$v/lib/$(basename $p).js"; echo "$p@$v $u sha384-$(curl -sL $u | openssl dgst -sha384 -binary | openssl base64 -A)"; done
```
Expected: two lines, each with a version and a `sha384-…` digest. Use them in Step 2.

- [ ] **Step 2: Rewrite `index.html`**

Keep the head's meta, the four existing script tags and their comments. Add, after the clipboard addon:

```html
<!-- Search over the buffer (the page's Search, k9s's filter as a browser
     does it) and serialization of it (Copy all, Save). -->
<script src="https://cdn.jsdelivr.net/npm/@xterm/addon-search@VERSION/lib/addon-search.js"
        integrity="sha384-DIGEST" crossorigin="anonymous" referrerpolicy="no-referrer"></script>
<script src="https://cdn.jsdelivr.net/npm/@xterm/addon-serialize@VERSION/lib/addon-serialize.js"
        integrity="sha384-DIGEST" crossorigin="anonymous" referrerpolicy="no-referrer"></script>
```

Replace the `<style>` block with:

```html
<style>
  html, body { height: 100%; margin: 0; background: #000; color: #cfcfcf;
    font: 12px/1.6 ui-monospace, SFMono-Regular, Menlo, Consolas, monospace; }
  body { display: flex; flex-direction: column; position: relative; }
  .motd { flex: none; }
  .message { padding: 0 8px; text-align: center; white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .message-note { background: #7cc4e8; color: #061e2b; }
  .message-warning { background: #f0c050; color: #2b1f00; }
  .message-caution { background: #e06060; color: #2b0000; }
  .message a { color: inherit; }
  .degraded { flex: none; font-size: 11px; color: #7a7a7a; background: #0c0c0c; border-bottom: 1px solid #1e1e1e; padding: 0 8px;
    white-space: nowrap; overflow: hidden; text-overflow: ellipsis; }
  .bar { flex: none; display: flex; align-items: center; gap: 8px; padding: 0 8px; height: 22px; color: #9a9a9a;
    white-space: nowrap; overflow: hidden; }
  .bar .left, .bar .right { display: flex; align-items: center; gap: 8px; min-width: 0; }
  .bar .centre { flex: 1; text-align: center; overflow: hidden; text-overflow: ellipsis; }
  .bar .right { margin-left: auto; }
  #top { border-bottom: 1px solid #2a4a6a; }
  #bottom { border-top: 1px solid #2a4a6a; }
  #origin { color: #7cc4e8; font-weight: 700; }
  #address, #banner a { color: #a8d070; text-decoration: underline dotted; text-underline-offset: 2px; }
  #address:hover, #banner a:hover { color: #fff; }
  .chip { background: #1e1e1e; color: #e8e8e8; border-radius: 3px; padding: 0 5px; }
  .chip.key { background: #3a2a00; color: #f0c050; font-weight: 700; }
  .copied { background: #203a20; color: #a8d070; }
  #stage { flex: 1; min-height: 0; display: flex; align-items: center; justify-content: center; position: relative; }
  #term { width: 100%; height: 100%; }
  #strip { display: flex; flex-wrap: wrap; gap: 6px; padding: 4px 8px; border-top: 1px solid #1e1e1e; background: #0a0a0a; }
  #strip[hidden] { display: none; }
  #strip button { font: inherit; color: #cfcfcf; background: #161616; border: 1px solid #333; border-radius: 3px; padding: 2px 8px; cursor: pointer; }
  #strip button:hover { background: #222; border-color: #555; }
  #strip button[hidden] { display: none; }
  #search { display: flex; gap: 6px; align-items: center; padding: 4px 8px; background: #0a0a0a; border-top: 1px solid #1e1e1e; }
  #search[hidden] { display: none; }
  #search input[type=text] { font: inherit; background: #000; color: #e8e8e8; border: 1px solid #333; padding: 2px 6px; width: 24em; }
  #search label { color: #9a9a9a; }
  #overlay { position: absolute; inset: 0; z-index: 10; background: rgba(0,0,0,0.85); display: flex; align-items: center; justify-content: center; }
  #overlay[hidden] { display: none; }
  #overlay pre { max-width: 90%; max-height: 90%; overflow: auto; color: #cfcfcf; margin: 0; }
  #overlay img { width: min(60vh, 90vw); image-rendering: pixelated; }
  #gone[hidden] { display: none; }
  #gone { position: absolute; inset: 0; z-index: 10; display: flex; flex-direction: column; align-items: center; justify-content: center; gap: 10px;
    background: rgba(0, 0, 0, 0.72); color: #cfcfcf; font-size: 13px; }
  #gone button { font: inherit; color: inherit; background: transparent; border: 1px solid #3a3a3a; border-radius: 3px; padding: 5px 14px; cursor: pointer; }
  #gone button:hover { background: #1a1a1a; border-color: #5a5a5a; }
  #where { color: #e8e8e8; }
  #why { color: #9a9a9a; }
</style>
```

Replace the body with:

```html
<body>
<div id="motd" class="motd"></div>
{{with .Notice}}<div class="degraded">{{.}}</div>{{end}}
<div id="top" class="bar">
  <div class="left"><span id="origin"></span></div>
  <div class="centre" id="title"></div>
  <div class="right"><a id="address" target="_blank" rel="noopener noreferrer"></a><a id="popout" hidden target="_blank" rel="noopener noreferrer" title="Open in a tab of its own">↗</a></div>
</div>
<div id="stage">
  <div id="term"></div>
  <div id="overlay" hidden></div>
  <div id="gone" hidden>
    <span id="where">{{.Origin}}</span>
    <span id="why">detached</span>
    <button id="again" type="button">reconnect</button>
  </div>
</div>
<div id="search" hidden>
  <input id="q" type="text" placeholder="search the terminal (regexp)" autocomplete="off">
  <label><input id="qcase" type="checkbox"> case</label>
  <span id="qcount"></span>
  <span class="chip">Enter next · Shift-Enter previous · Esc close</span>
</div>
<div id="strip" hidden>
  <button data-cmd="copy"><span class="chip key">c</span> Copy all</button>
  <button data-cmd="save"><span class="chip key">s</span> Save</button>
  <button data-cmd="search"><span class="chip key">/</span> Search</button>
  <button data-cmd="logs"><span class="chip key">l</span> Logs</button>
  <button data-cmd="qr"><span class="chip key">q</span> QR</button>
  <button data-cmd="restart" hidden><span class="chip key">r</span> Restart</button>
  <button data-cmd="detach"><span class="chip key">d</span> Detach</button>
  <button data-cmd="exit"><span class="chip key">x</span> Exit</button>
  <button data-cmd="close"><span class="chip key">esc</span> Close</button>
</div>
<div id="bottom" class="bar">
  <div class="left"><span id="hint"><span class="chip key">^K</span> commands</span><span id="armed" hidden></span><span id="copied" class="chip copied" hidden>copied</span></div>
  <div class="centre" id="banner"></div>
  <div class="right"><span id="host"></span><span id="viewers"></span><span id="size"></span></div>
</div>
<script>
(() => {
  const STDIN = 0, STDOUT = 1, STDERR = 2, ERROR = 3, RESIZE = 4;
  const $ = (id) => document.getElementById(id);
  const encoder = new TextEncoder(), decoder = new TextDecoder();

  // The terminal is the program's own stream now: xterm selects, scrolls and
  // encodes the mouse as a terminal does, because it sees every mode the
  // program sets. Nothing of the frame is drawn into it.
  const term = new Terminal({
    cursorBlink: true,
    convertEol: true,
    fontSize: 13,
    scrollback: 10000,
    linkHandler: {
      activate(event, uri) { event.preventDefault(); window.open(uri, '_blank', 'noopener,noreferrer'); },
    },
  });
  const fit = new FitAddon.FitAddon();
  term.loadAddon(fit);
  const search = new SearchAddon.SearchAddon();
  term.loadAddon(search);
  const serialize = new SerializeAddon.SerializeAddon();
  term.loadAddon(serialize);
  term.onTitleChange((title) => { document.title = title; });
  term.open($('term'));
  try { const webgl = new WebglAddon.WebglAddon(); webgl.onContextLoss(() => webgl.dispose()); term.loadAddon(webgl); }
  catch (e) { console.warn('attach: falling back to the DOM renderer', e); }
  try { term.loadAddon(new ClipboardAddon.ClipboardAddon()); }
  catch (e) { console.warn('attach: clipboard addon failed to load', e); }

  // The chrome, as the session says it: OSC 7770 carries JSON in base64.
  // Handled before the bytes after it, so the snapshot that follows the
  // first message lands in a terminal already the pane's size. Returning
  // true tells xterm the sequence is consumed.
  let state = {};
  const linkify = (text) => {
    const frag = document.createDocumentFragment();
    const re = /https?:\/\/[^\s<>"']+/g;
    let last = 0, m;
    while ((m = re.exec(text))) {
      frag.append(text.slice(last, m.index));
      const a = document.createElement('a');
      a.href = m[0]; a.textContent = m[0]; a.target = '_blank'; a.rel = 'noopener noreferrer';
      frag.append(a);
      last = m.index + m[0].length;
    }
    frag.append(text.slice(last));
    return frag;
  };
  const draw = () => {
    const c = state;
    $('origin').textContent = c.origin || '';
    $('title').textContent = c.title || c.subtitle || '';
    const addr = $('address'), pop = $('popout');
    addr.hidden = !c.address || c.embedded;
    addr.textContent = c.address || ''; addr.href = c.address || '#';
    pop.hidden = !(c.address && c.embedded); pop.href = c.address || '#';
    $('banner').replaceChildren(linkify(c.banner || ''));
    $('host').textContent = c.host || '';
    $('viewers').textContent = c.viewers ? `(${c.viewers} viewer${c.viewers === 1 ? '' : 's'})` : '';
    $('size').textContent = c.cols ? `[${c.cols}×${c.rows}]` : '';
    $('bottom').hidden = !!c.embedded;
    const motd = $('motd');
    motd.replaceChildren(...(c.motd || []).map((m) => {
      const d = document.createElement('div');
      d.className = 'message' + (m.severity ? ' message-' + m.severity : '');
      if (m.severity) { const l = document.createElement('b'); l.textContent = m.severity.toUpperCase() + ' '; d.append(l); }
      d.append(linkify(m.text));
      return d;
    }));
    document.querySelector('[data-cmd=restart]').hidden = !c.restartable;
    if (c.cols && c.rows && (term.cols !== c.cols || term.rows !== c.rows)) term.resize(c.cols, c.rows);
    publish();
  };
  term.parser.registerOscHandler(7770, (data) => {
    try { state = JSON.parse(decoder.decode(Uint8Array.from(atob(data), (ch) => ch.charCodeAt(0)))); draw(); }
    catch (e) { console.warn('attach: bad chrome', e); }
    return true;
  });

  // The grid the multiview panel reads: rows, columns and the cell.
  const publish = () => {
    const screen = document.querySelector('.xterm-screen');
    if (!screen || !term.rows || !term.cols) return;
    const r = screen.getBoundingClientRect();
    const d = document.documentElement.dataset;
    d.rows = String(term.rows); d.cols = String(term.cols);
    d.cellW = (r.width / term.cols).toFixed(4); d.cellH = (r.height / term.rows).toFixed(4);
  };

  // The socket, as before: the routing parameter travels in location.search.
  const proto = location.protocol === 'https:' ? 'wss:' : 'ws:';
  let embedded = false;
  try { embedded = window.self !== window.top; } catch (e) { embedded = true; }
  const path = embedded ? '/attach/embedded' : '/attach';
  const ws = new WebSocket(proto + '//' + location.host + path + location.search, ['v4.channel.k8s.io']);
  ws.binaryType = 'arraybuffer';
  const send = (channel, bytes) => {
    if (ws.readyState !== WebSocket.OPEN) return;
    const frame = new Uint8Array(bytes.length + 1);
    frame[0] = channel; frame.set(bytes, 1);
    ws.send(frame);
  };
  // What this window could show, proposed rather than applied: the session
  // settles the pane on the smallest viewer and says so in the chrome, and
  // that is the size this terminal takes.
  const sendSize = () => {
    const dims = fit.proposeDimensions();
    if (dims && dims.cols > 0 && dims.rows > 0) send(RESIZE, encoder.encode(JSON.stringify({ Width: dims.cols, Height: dims.rows })));
  };
  ws.onmessage = (event) => {
    const frame = new Uint8Array(event.data);
    if (frame.length < 2) { if (frame[0] === STDOUT) sendSize(); return; }
    const body = frame.subarray(1);
    switch (frame[0]) {
      case STDOUT: case STDERR: term.write(body); break;
      case ERROR:
        try { const status = JSON.parse(decoder.decode(body)); if (status.status !== 'Success') gone(status.message || 'attach failed'); }
        catch { /* not JSON */ }
        break;
    }
  };

  // The overlay for a socket that is gone: as before.
  const overlay = $('gone'), why = $('why'), again = $('again');
  again.onclick = () => location.reload();
  const gone = (reason) => {
    if (!overlay.hidden) return;
    why.textContent = reason || 'detached';
    overlay.hidden = false;
    again.hidden = true;
    if (reason) return;
    fetch('/alive' + location.search, { cache: 'no-store' })
      .then((r) => (r.ok ? r.text() : ''))
      .then((offer) => { why.textContent = offer === 'reconnect' ? 'detached' : 'ended'; again.textContent = offer; again.hidden = !offer; })
      .catch(() => { why.textContent = 'ended'; });
  };
  ws.onclose = () => { clearInterval(beat); gone(); };

  // Keys. xterm encodes them; they go to the program as bytes. ^C and ^D are
  // held until pressed twice when nothing could bring the run back; the
  // strip, when open, takes every key.
  let armed = '', armTimer = 0;
  const armGrace = 3000;
  const sendBytes = (data) => send(STDIN, encoder.encode(data));
  term.onData((data) => {
    if (!$('strip').hidden || !$('search').hidden || !$('overlay').hidden) return;
    const ctl = data === '\x03' ? 'C' : data === '\x04' ? 'D' : '';
    if (ctl && !state.restartable && !state.embedded) {
      if (armed !== ctl) {
        armed = ctl;
        $('armed').textContent = `^${ctl} again to send it`; $('armed').hidden = false;
        clearTimeout(armTimer); armTimer = setTimeout(disarm, armGrace);
        return;
      }
      disarm();
    } else if (armed) disarm();
    sendBytes(data);
  });
  const disarm = () => { armed = ''; $('armed').hidden = true; clearTimeout(armTimer); };

  // The command strip. ^K opens it, as the frame's ^K does on a console;
  // the browser claims that chord for its address bar, so the page takes it
  // first. With it open, single keys run the commands and nothing reaches
  // the program.
  const strip = $('strip');
  const openStrip = (open) => { strip.hidden = !open; if (!open) term.focus(); };
  const commands = {
    copy: () => { const text = serialize.serialize().replace(/\x1b\[[0-9;?]*[A-Za-z]/g, ''); navigator.clipboard.writeText(text).then(() => flash('copied')).catch(() => flash('copy refused')); },
    save: () => {
      const text = serialize.serialize().replace(/\x1b\[[0-9;?]*[A-Za-z]/g, '');
      const a = document.createElement('a');
      a.href = URL.createObjectURL(new Blob([text], { type: 'text/plain' }));
      a.download = `${(state.origin || 'terminal').replace(/[^\w.-]+/g, '_')}-${new Date().toISOString().replace(/[:.]/g, '-')}.txt`;
      a.click(); URL.revokeObjectURL(a.href); flash('saved');
    },
    search: () => { openStrip(false); $('search').hidden = false; $('q').focus(); $('q').select(); },
    logs: () => fetch('/attach/logs' + location.search).then((r) => r.text()).then((t) => { const pre = document.createElement('pre'); pre.textContent = t || 'nothing logged yet'; show(pre); }),
    qr: () => { const img = document.createElement('img'); img.src = '/attach/qr.svg' + location.search; img.alt = state.address || ''; show(img); },
    restart: () => fetch('/attach/restart' + location.search, { method: 'POST' }).then(() => flash('restarting')),
    detach: () => { ws.close(); },
    exit: () => { if (confirm('End the whole run, for every viewer?')) fetch('/attach/exit' + location.search, { method: 'POST' }); },
    close: () => openStrip(false),
  };
  const keys = { c: 'copy', s: 'save', '/': 'search', l: 'logs', q: 'qr', r: 'restart', d: 'detach', x: 'exit', Escape: 'close' };
  strip.addEventListener('click', (e) => { const b = e.target.closest('button'); if (b) { commands[b.dataset.cmd](); if (b.dataset.cmd !== 'search') openStrip(false); } });
  term.attachCustomKeyEventHandler((e) => {
    if (e.type !== 'keydown') return true;
    if (e.key === 'k' && e.ctrlKey && !e.metaKey && !e.altKey && !e.shiftKey) { e.preventDefault(); openStrip(strip.hidden); return false; }
    if (!strip.hidden) {
      e.preventDefault();
      const cmd = keys[e.key];
      if (cmd && !(cmd === 'restart' && !state.restartable)) { commands[cmd](); if (cmd !== 'search') openStrip(false); }
      return false;
    }
    if (!$('overlay').hidden && e.key === 'Escape') { e.preventDefault(); show(null); return false; }
    return true;
  });
  // The chip the last copy or save left, for a moment.
  let flashTimer = 0;
  const flash = (text) => { const c = $('copied'); c.textContent = text; c.hidden = false; clearTimeout(flashTimer); flashTimer = setTimeout(() => { c.hidden = true; }, 2500); };
  // Logs and the code, over the terminal; a click or Escape closes.
  const show = (el) => { const o = $('overlay'); o.replaceChildren(...(el ? [el] : [])); o.hidden = !el; if (!el) term.focus(); };
  $('overlay').addEventListener('click', () => show(null));

  // Search: xterm's addon over the whole buffer, with the matches
  // highlighted. Enter and Shift-Enter step; Escape closes.
  const q = $('q'), qcase = $('qcase'), qcount = $('qcount');
  const opts = () => ({ regex: true, caseSensitive: qcase.checked, decorations: { matchBackground: '#3a2a00', matchBorder: '#f0c050', matchOverviewRuler: '#f0c050', activeMatchBackground: '#f0c050', activeMatchColorOverviewRuler: '#fff' } });
  search.onDidChangeResults((r) => { qcount.textContent = r && r.resultCount ? `${r.resultIndex + 1} of ${r.resultCount}` : (q.value ? 'no matches' : ''); });
  const find = (back) => { try { (back ? search.findPrevious : search.findNext).call(search, q.value, opts()); } catch { qcount.textContent = 'bad pattern'; } };
  q.addEventListener('input', () => find(false));
  qcase.addEventListener('change', () => find(false));
  q.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') { e.preventDefault(); find(e.shiftKey); }
    if (e.key === 'Escape') { e.preventDefault(); search.clearDecorations(); $('search').hidden = true; q.value = ''; qcount.textContent = ''; term.focus(); }
  });

  new ResizeObserver(() => { sendSize(); publish(); }).observe($('stage'));
  const beat = setInterval(sendSize, 30000);
  term.focus();
})();
</script>
</body>
```

Where `VERSION`/`DIGEST` appear, use Step 1's values.

- [ ] **Step 3: Pin the addons in the README**

In `README.md`'s acknowledgements, the xterm bullet lists its pins; add `@xterm/addon-search@VERSION` and `@xterm/addon-serialize@VERSION` to it in the same code-span form.

Run: `go test ./ -run Acknowledgements`
Expected: PASS (fails until the pins are listed).

- [ ] **Step 4: Build and try it by hand**

Run: `make binary && ./tunneld --log-level=debug sh` on this machine, open the printed address in Chrome, and check each of these, noting each result in the PR body:

1. The top bar shows the origin left, the address right as a link; the bottom bar shows the banner, host, `1 viewer`, and the size.
2. Select by drag, double-click a word, triple-click a line, Shift-click to extend; Cmd/Ctrl+C copies; the right-click menu copies.
3. `seq 1 3000`, then wheel up through history; drag past the top edge scrolls.
4. `^K` opens the strip; `/` then `3000` finds and highlights; Enter steps; Esc closes.
5. `^K c` says `copied` and the clipboard holds the buffer; `^K s` downloads a file.
6. `^K l` shows tunneld's logs; `^K q` shows a code; Esc closes each.
7. `vi` draws full-screen; the mouse selects in vi only when vi asks (`:set mouse=a`); `:q`.
8. `^K r` restarts; `^K d` shows `detached` with `reconnect`; `^K x` asks, then the run ends and the page says `ended`.
9. Typing with the strip open reaches nothing.
10. A second tab joins mid-`vi`: it shows vi's screen; narrow it, and both tabs' terminals shrink to the pane.
11. Close the laptop lid or kill the socket: `detached`, `reconnect` works and shows history again.
12. `npx tunneld :3000 sh` in the multiview: the tile shows the ↗ pop-out and no bottom bar.
13. A `TUNNEL_PASSPHRASE`-less public tunnel from tunnel.pizza shows the motd strip at the top.

- [ ] **Step 5: Commit**

```bash
git add v1alpha1/attach/index.html README.md
git commit -m "feat(attach): the page draws the chrome and runs xterm on the program's stream (#274)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 6: The console's commands as one table

**Files:**
- Modify: `v1alpha1/attach/frame.go` (`hint`'s command branch, `commanded`)
- Test: `v1alpha1/attach/frame_test.go`

**Interfaces:**
- Produces: `type action struct { key rune; label string; when func(frame) bool; run func(frame) (frame, tea.Cmd) }`, `var commands []action`, `func (f frame) offered() []action`.

- [ ] **Step 1: Write the failing test**

Append to `frame_test.go`:

```go
// TestEveryCommandIsOfferedAndHandled pins the one table: every key the
// bottom row offers has a handler, and every handler is offered somewhere.
func TestEveryCommandIsOfferedAndHandled(t *testing.T) {
	h := consoleHarness(t)
	h.press(t, commandKey)
	hint := stripSGR(bottomOf(h))
	for _, a := range h.f.offered() {
		if !strings.Contains(hint, " "+string(a.key)+" ") || !strings.Contains(hint, a.label) {
			t.Errorf("%q %q is in the table but not the hint %q", string(a.key), a.label, hint)
		}
		if a.run == nil {
			t.Errorf("%q has no handler", string(a.key))
		}
	}
	for _, a := range commands {
		if a.when == nil {
			t.Errorf("%q says nothing about when it is offered", string(a.key))
		}
	}
	if len(h.f.offered()) != strings.Count(hint, "")-1 && len(h.f.offered()) < 5 {
		t.Errorf("offered %d commands on a console; want at least d x l q [ m", len(h.f.offered()))
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./v1alpha1/attach/ -run TestEveryCommandIsOfferedAndHandled`
Expected: FAIL, `h.f.offered undefined`.

- [ ] **Step 3: The table**

In `frame.go`, above `commanded`:

```go
// action is one entry of the commands menu: its key, its label in the bottom
// row, when it is offered, and what it does. One table, read by hint and
// commanded both, so a key is never offered without a handler or handled
// without being offered.
type action struct {
	key   rune
	label string
	when  func(frame) bool
	run   func(frame) (frame, tea.Cmd)
}

var always = func(frame) bool { return true }

var commands = []action{
	{'d', "detach", always, func(f frame) (frame, tea.Cmd) { return f, tea.Quit }},
	{'x', "exit", always, func(f frame) (frame, tea.Cmd) { f.exiting = true; return f, tea.Quit }},
	{'r', "restart", func(f frame) bool { return f.sess.restartable() }, func(f frame) (frame, tea.Cmd) {
		go f.sess.restart()
		return f, nil
	}},
	{'l', "logs", always, func(f frame) (frame, tea.Cmd) { f.logs = true; return f, nil }},
	{'q', "qr", always, func(f frame) (frame, tea.Cmd) { f.qr = true; return f, nil }},
	{'[', "scroll", frame.console, func(f frame) (frame, tea.Cmd) {
		f.reading, f.selecting, f.selected, f.follow = true, false, false, true
		return f.scroll(0), nil
	}},
	{'m', "mouse", frame.console, func(f frame) (frame, tea.Cmd) {
		f.mouseOff = !f.mouseOff
		f.selecting, f.selected = false, false
		return f, nil
	}},
}

// offered is the commands this frame offers now.
func (f frame) offered() []action {
	var out []action
	for _, a := range commands {
		if a.when(f) {
			out = append(out, a)
		}
	}
	return out
}
```

`commanded` becomes:

```go
func (f frame) commanded(k tea.Key) (tea.Model, tea.Cmd) {
	f.command = false
	for _, a := range f.offered() {
		if k.Code == a.key {
			return a.run(f)
		}
	}
	// Escape, or anything unbound: the mode closes and the keystroke is spent
	// on closing it.
	return f, nil
}
```

and `hint`'s command branch (from `// r only where it works` to the end) becomes:

```go
	var b strings.Builder
	for _, a := range f.offered() {
		b.WriteString(chipStyle.Styled(" " + string(a.key) + " ") + hintStyle.Styled(" " + a.label + " "))
	}
	// esc gives up its chip on the console so everything fits 80 columns;
	// any unbound key cancels anyway.
	if !f.console() {
		b.WriteString(chipStyle.Styled(" esc ") + hintStyle.Styled(" cancel "))
	}
	return b.String()
```

(`f.follow` is added in Task 8; until then set only `f.reading, f.selecting, f.selected`.) Keep each old handler's comment above its table entry, shortened to the constraint.

- [ ] **Step 4: Run the package**

Run: `go test ./v1alpha1/attach/`
Expected: PASS.

- [ ] **Step 5: Commit**

```bash
git add v1alpha1/attach/frame.go v1alpha1/attach/frame_test.go
git commit -m "refactor(attach): the console's commands as one table (#274)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 7: Word, line and extended selection on the console

**Files:**
- Modify: `v1alpha1/attach/selection.go`, `v1alpha1/attach/frame.go` (`pressed`, new fields)
- Test: `v1alpha1/attach/selection_test.go`, `v1alpha1/attach/frame_test.go`

**Interfaces:**
- Produces: `func wordAt(buf uv.ScreenBuffer, pos uv.Position) selection`, `func rowOf(buf uv.ScreenBuffer, y int) selection`; frame fields `lastPress time.Time`, `lastAt uv.Position`, `clicks int`; `const clickWindow = 400 * time.Millisecond`.

- [ ] **Step 1: Write the failing tests**

Append to `selection_test.go`:

```go
// TestWordAtAndRowOf pin what a double and a triple click select.
func TestWordAtAndRowOf(t *testing.T) {
	buf := uv.NewScreenBuffer(20, 2)
	uv.NewStyledString("foo bar-baz.qux x").Draw(buf, buf.Bounds())
	if got := wordAt(buf, uv.Pos(6, 0)); got != (selection{anchor: uv.Pos(4, 0), head: uv.Pos(14, 0)}) {
		t.Errorf("wordAt(6,0) = %+v, want bar-baz.qux", got)
	}
	if got := wordAt(buf, uv.Pos(3, 0)); !got.empty() {
		t.Errorf("wordAt on a space = %+v, want nothing", got)
	}
	if got := rowOf(buf, 0); got != (selection{anchor: uv.Pos(0, 0), head: uv.Pos(19, 0)}) {
		t.Errorf("rowOf(0) = %+v, want the whole row", got)
	}
}
```

Append to `frame_test.go`:

```go
// TestClicksSelectWordsAndRows pins the console's clicks: a second press on
// the same cell within the window selects the word, a third the row, and a
// Shift-press extends what is selected to the click.
func TestClicksSelectWordsAndRows(t *testing.T) {
	h := consoleHarness(t)
	if _, err := h.s.em.WriteString("hello wide world\r\nsecond"); err != nil {
		t.Fatal(err)
	}
	press := func(x, y int, mod tea.KeyMod) tea.Cmd {
		h.mouse(t, tea.MouseClickMsg{X: x, Y: y, Button: tea.MouseLeft, Mod: mod})
		return h.mouse(t, tea.MouseReleaseMsg{X: x, Y: y, Button: tea.MouseLeft})
	}
	press(8, 1, 0)
	cmd := press(8, 1, 0)
	if cmd == nil || !strings.HasPrefix(string(cmd().(tea.RawMsg).Msg.(string)), "\x1b]52;c;") {
		t.Fatalf("a double click copied nothing")
	}
	if got := h.f.sel.text(h.f.composed()); got != "wide" {
		t.Errorf("double click selected %q, want wide", got)
	}
	press(8, 1, 0)
	if got := h.f.sel.text(h.f.composed()); got != "hello wide world" {
		t.Errorf("triple click selected %q, want the row", got)
	}
	press(2, 2, tea.ModShift)
	if got := h.f.sel.text(h.f.composed()); got != "hello wide world\nsec" {
		t.Errorf("shift-click selected %q, want the row extended to the click", got)
	}
	h.f.lastPress = h.f.lastPress.Add(-2 * clickWindow)
	press(1, 1, 0)
	if h.f.selected {
		t.Error("a single click after the window left a selection")
	}
}
```

- [ ] **Step 2: Run them to make sure they fail**

Run: `go test ./v1alpha1/attach/ -run 'TestWordAtAndRowOf|TestClicksSelectWordsAndRows'`
Expected: FAIL, `undefined: wordAt`.

- [ ] **Step 3: Implement**

Append to `selection.go`:

```go
// wordChar is a character a double-click's word runs through: letters,
// digits and the punctuation that joins paths, hosts and flags.
func wordChar(c *uv.Cell) bool {
	if c == nil || c.Content == "" {
		return false
	}
	r := []rune(c.Content)[0]
	return unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_-./:@", r)
}

// wordAt is the word under pos, or an empty selection when there is none.
func wordAt(buf uv.ScreenBuffer, pos uv.Position) selection {
	if !wordChar(buf.CellAt(pos.X, pos.Y)) {
		return selection{anchor: pos, head: pos}
	}
	from, to := pos.X, pos.X
	for from > 0 && wordChar(buf.CellAt(from-1, pos.Y)) {
		from--
	}
	w := buf.Bounds().Dx()
	for to+1 < w && wordChar(buf.CellAt(to+1, pos.Y)) {
		to++
	}
	return selection{anchor: uv.Pos(from, pos.Y), head: uv.Pos(to, pos.Y)}
}

// rowOf is row y whole.
func rowOf(buf uv.ScreenBuffer, y int) selection {
	return selection{anchor: uv.Pos(0, y), head: uv.Pos(buf.Bounds().Dx()-1, y)}
}
```

(add `"unicode"` to its imports.) In `frame.go`, add fields beside `sel`:

```go
	// lastPress, lastAt and clicks count presses on one cell within
	// clickWindow: two select the word, three the row. Bubble Tea's events
	// carry no count of their own.
	lastPress time.Time
	lastAt    uv.Position
	clicks    int
```

and `const clickWindow = 400 * time.Millisecond` beside `armGrace`. `pressed` becomes:

```go
func (f frame) pressed(m tea.MouseClickMsg) frame {
	if m.Button != tea.MouseLeft {
		return f
	}
	if !uv.Pos(m.X, m.Y).In(f.pane()) {
		f.selected, f.selecting, f.clip, f.clicks = false, false, "", 0
		return f
	}
	pos := f.onPane(m.X, m.Y)
	// Shift extends what is selected to the click, as a terminal does.
	if m.Mod&tea.ModShift != 0 && (f.selected || f.selecting) {
		f.sel.head, f.selecting, f.selected, f.clip = pos, true, false, ""
		return f
	}
	now := time.Now()
	if now.Sub(f.lastPress) <= clickWindow && pos == f.lastAt {
		f.clicks++
	} else {
		f.clicks = 1
	}
	f.lastPress, f.lastAt = now, pos
	f.selected, f.clip = false, ""
	switch f.clicks {
	case 2:
		f.sel = wordAt(f.composed(), pos)
	case 3:
		f.sel = rowOf(f.composed(), pos.Y)
	default:
		f.clicks = 1
		f.sel = selection{anchor: pos, head: pos}
	}
	f.selecting = true
	return f
}
```

and `released` keeps a word or row selection's head where the press put it: change `f.sel.head = f.onPane(m.X, m.Y)` to run only when `f.clicks == 1`.

- [ ] **Step 4: Run the package**

Run: `go test ./v1alpha1/attach/`
Expected: PASS, `TestDraggingSelectsAndCopies` and `TestAClickIsStillAClick` included.

- [ ] **Step 5: Commit**

```bash
git add v1alpha1/attach/selection.go v1alpha1/attach/selection_test.go v1alpha1/attach/frame.go v1alpha1/attach/frame_test.go
git commit -m "feat(attach): word, row and extended selection on the console (#274)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 8: Scrollback mode: follow, filter, clear

**Files:**
- Modify: `v1alpha1/attach/frame.go` (`read`, `scroll`, `composed`, `hint`'s reading branch, `Update`'s paneMsg; new fields `follow`, `filter`, `filtering`, `pattern`, `cleared`)
- Test: `v1alpha1/attach/frame_test.go`

**Interfaces:**
- Produces: `func (f frame) filtered() (rows []int, total int)`; frame fields as above.

- [ ] **Step 1: Write the failing test**

Append to `frame_test.go`:

```go
// TestScrollbackFollowsFiltersAndClears pins the k9s-style keys: the view
// follows new output until the reader scrolls up, s and G toggle it and the
// row says so; / filters the history by a pattern, ! inverting it, with the
// count in the row; C forgets the history up to now.
func TestScrollbackFollowsFiltersAndClears(t *testing.T) {
	h := consoleHarness(t)
	h.f.width, h.f.height = 40, 8
	h.mouse(t, tea.WindowSizeMsg{Width: 40, Height: 8})
	var b strings.Builder
	for i := 1; i <= 20; i++ {
		fmt.Fprintf(&b, "line %02d %s\r\n", i, map[bool]string{true: "even", false: "odd"}[i%2 == 0])
	}
	write := func(s string) {
		t.Helper()
		if _, err := (&sink{s: h.s}).Write([]byte(s)); err != nil {
			t.Fatal(err)
		}
		h.mouse(t, paneMsg{})
	}
	write(b.String())
	h.enterScrollback(t)
	if !strings.Contains(stripSGR(bottomOf(h)), "follow:on") {
		t.Errorf("entering the mode shows %q, want follow:on", stripSGR(bottomOf(h)))
	}
	write("line 21 odd\r\n")
	if !strings.Contains(stripSGR(h.f.View().Content), "line 21") {
		t.Error("following, new output is not in view")
	}
	h.press(t, tea.Key{Code: tea.KeyUp})
	if !strings.Contains(stripSGR(bottomOf(h)), "follow:off") {
		t.Errorf("scrolling up shows %q, want follow:off", stripSGR(bottomOf(h)))
	}
	write("line 22 even\r\n")
	if strings.Contains(stripSGR(h.f.View().Content), "line 22") {
		t.Error("not following, new output moved the view")
	}
	h.press(t, typing('G'))
	if !strings.Contains(stripSGR(bottomOf(h)), "follow:on") || !strings.Contains(stripSGR(h.f.View().Content), "line 22") {
		t.Error("G did not follow again")
	}

	h.press(t, typing('/'))
	for _, r := range "even" {
		h.press(t, typing(r))
	}
	h.press(t, tea.Key{Code: tea.KeyEnter})
	view := stripSGR(h.f.View().Content)
	if strings.Contains(view, "odd") || !strings.Contains(view, "even") {
		t.Errorf("filtered view %q shows odd lines", view)
	}
	if !strings.Contains(stripSGR(bottomOf(h)), " of ") {
		t.Errorf("the row %q does not count matches", stripSGR(bottomOf(h)))
	}
	h.press(t, typing('/'))
	for _, r := range "!even" {
		h.press(t, typing(r))
	}
	h.press(t, tea.Key{Code: tea.KeyEnter})
	if view := stripSGR(h.f.View().Content); strings.Contains(view, "even") {
		t.Errorf("inverted filter view %q shows even lines", view)
	}
	h.press(t, tea.Key{Code: tea.KeyEscape})
	if h.f.pattern != "" || !h.f.reading {
		t.Errorf("escape in a filter: pattern %q reading %v; want the filter cleared and the mode kept", h.f.pattern, h.f.reading)
	}

	h.press(t, typing('C'))
	write("line 23 odd\r\n")
	if view := stripSGR(h.f.View().Content); strings.Contains(view, "line 01") || !strings.Contains(view, "line 23") {
		t.Errorf("after C the view %q still has old lines or lacks new ones", view)
	}
}
```

- [ ] **Step 2: Run it to make sure it fails**

Run: `go test ./v1alpha1/attach/ -run TestScrollbackFollowsFiltersAndClears`
Expected: FAIL (no `follow:on` in the row).

- [ ] **Step 3: Implement**

Fields on `frame`, beside `reading`:

```go
	// follow keeps the reading view on the newest output; scrolling up turns
	// it off, s toggles it, G turns it on. pattern filters the history to
	// matching rows while reading, "" for none; filtering is the pattern
	// being typed, into filter. cleared is how many history lines C has
	// forgotten for this viewer; the emulator keeps them.
	follow    bool
	pattern   string
	filtering bool
	filter    string
	cleared   int
```

In `Update`'s `paneMsg` case, before `return`:

```go
		if f.reading && f.follow {
			f = f.scroll(f.sess.history() + f.pane().Dy())
		}
```

`scroll` sets `f.follow = f.top >= kept` after clamping (the foot is following; above it is not), except when called with step 0. `read` gains, before its `switch`:

```go
	if f.filtering {
		switch {
		case k.Code == tea.KeyEnter:
			f.filtering, f.pattern = false, f.filter
		case k.Code == tea.KeyEscape:
			f.filtering, f.filter = false, ""
		case k.Code == tea.KeyBackspace:
			if n := len(f.filter); n > 0 {
				f.filter = f.filter[:n-1]
			}
		case k.Text != "":
			f.filter += k.Text
		}
		return f, nil
	}
```

and new cases:

```go
	case k.Code == tea.KeyEscape && f.pattern != "":
		f.pattern = ""
	case key == "s":
		f.follow = !f.follow
		if f.follow {
			f = f.scroll(f.sess.history() + page)
		}
	case key == "/":
		f.filtering, f.filter = true, ""
	case key == "C":
		f.cleared = f.sess.history()
		f = f.scroll(f.sess.history() + page)
```

(`G` already scrolls to the end, which sets follow on.) `filtered` on the frame:

```go
// filtered is which history rows (from cleared on) match the pattern, and
// how many there were: a case-insensitive regexp, inverted by a leading !.
// A pattern that does not compile matches nothing.
func (f frame) filtered() (rows []int, total int) {
	buf := f.sess.transcript()
	total = buf.Bounds().Dy() - f.cleared
	pat, invert := f.pattern, false
	if strings.HasPrefix(pat, "!") {
		pat, invert = pat[1:], true
	}
	re, err := regexp.Compile("(?i)" + pat)
	if err != nil {
		return nil, total
	}
	for y := f.cleared; y < buf.Bounds().Dy(); y++ {
		line := rowOf(buf, y).text(buf)
		if re.MatchString(line) != invert {
			rows = append(rows, y)
		}
	}
	return rows, total
}
```

`composed`'s `f.scrolled` case becomes:

```go
	case f.scrolled && f.pattern != "":
		rows, _ := f.filtered()
		buf := f.sess.transcript()
		start := max(0, min(f.top-f.cleared, len(rows)-pane.Dy()))
		for i := 0; i < pane.Dy() && start+i < len(rows); i++ {
			for x := range pane.Dx() {
				pixels.SetCell(x, i, buf.CellAt(x, rows[start+i]))
			}
		}
	case f.scrolled:
		f.sess.drawHistory(pixels, pixels.Bounds(), max(f.top, f.cleared))
```

`hint`'s reading branch becomes:

```go
	if f.reading {
		if f.filtering {
			return chipStyle.Styled(" / ") + hintStyle.Styled(" "+f.filter+"▏ enter apply · esc cancel ")
		}
		var scroll, count string
		if !f.sess.altScreen() {
			scroll = hintStyle.Styled(" ↑↓ ")
		}
		if f.pattern != "" {
			rows, total := f.filtered()
			count = chipStyle.Styled(fmt.Sprintf(" %d of %d lines ", len(rows), total))
		}
		follow := "off"
		if f.follow {
			follow = "on"
		}
		return chipStyle.Styled(" scrollback ") + scroll +
			chipStyle.Styled(" s ") + hintStyle.Styled(" follow:"+follow+" ") +
			chipStyle.Styled(" / ") + hintStyle.Styled(" filter ") + count +
			chipStyle.Styled(" C ") + hintStyle.Styled(" clear ") +
			chipStyle.Styled(" c ") + hintStyle.Styled(" copy ") +
			chipStyle.Styled(" f ") + hintStyle.Styled(" bare ") +
			chipStyle.Styled(" esc ") + hintStyle.Styled(" live ")
	}
```

Leaving the mode (`esc`/`q`) also clears `pattern`, `filter`, `filtering`, `cleared`. Add `"regexp"` to `frame.go`'s imports.

- [ ] **Step 4: Run the package**

Run: `go test ./v1alpha1/attach/`
Expected: PASS, the existing scrollback tests included (`TestScrollbackModeCopiesEverything`, `TestCopyingTooMuchHistoryCopiesItsEnd`). An 80-column console's bottom row is now wider than before; `TestScrollbackModeReleasesTheMouse` and friends assert on content, not width.

- [ ] **Step 5: Commit**

```bash
git add v1alpha1/attach/frame.go v1alpha1/attach/frame_test.go
git commit -m "feat(attach): scrollback mode follows, filters and clears (#274)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```

---

### Task 9: Docs

**Files:**
- Modify: `docs/reference.md` (the selecting-and-copying section, ~536-555, and the commands list wherever `^K` is described), `CONTRIBUTING.md` (file map: `tab.go`, `snapshot.go`)

- [ ] **Step 1: Rewrite the section**

Replace the two paragraphs from "The frame asks whatever it is drawn on for the mouse" through "…any key that is not a command still cancels." with:

```markdown
**In a browser tab** the terminal is xterm on the program's own stream, so
selecting and copying are the browser's: drag, double-click a word,
triple-click a line, Shift-click to extend, `Cmd`/`Ctrl+C` or the right-click
menu to copy. The wheel scrolls 10,000 lines of history, which a tab opened
later gets too. Around the terminal the page draws the address (a link),
the messages of the day, and the bottom bar; `Ctrl+K` opens the commands:
`c` copies the whole buffer, `s` saves it as a file, `/` searches it (a
regexp, with the matches highlighted), `l` shows tunneld's logs, `q` the
address as a code, `r` restarts the program (where it can be), `d` detaches
this tab, `x` ends the run for everyone. `Ctrl-C` and `Ctrl-D` are held
until pressed twice on a program that could not be brought back.

**On the console** the frame asks your terminal for the mouse, which is what
makes the wheel reach it, and does its own selecting: drag, double-click a
word, triple-click a row, Shift-click to extend; copied to your clipboard on
release through OSC 52, which iTerm2 honours once "Applications in terminal
may access clipboard" is on, VS Code's terminal honours as is, and
Terminal.app does not. Inside tmux the copy goes twice, plain and in tmux's
passthrough, and the border says `sent to tmux`; inside screen it goes in
screen's own wrapping. A copy longer than a terminal will take (74,994 bytes
encoded) is not sent, and the border says `too large to copy`.

For more than a screen the console has **scrollback mode** (`Ctrl+K` then
`[`). It releases the mouse, so your terminal selects natively, and typing
reaches nobody. The arrows, `PgUp`/`PgDn`, `Home`/`End` and
`j`/`k`/`b`/space/`g`/`G` move through the history. It follows new output
until you scroll up; `s` toggles that and the border says `follow:on` or
`off`, `G` turns it back on. `/` filters the history to the rows matching a
pattern (a case-insensitive regexp; `!` first inverts it), with the count in
the border, and `esc` clears it. `C` forgets the history up to now for this
viewer. `c` copies all of it (or, past what a terminal will take, the end of
it, and the border says `copied (end)`); `f` drops the border so a native
selection picks up none of it; `esc` or `q` is the live screen again.
```

- [ ] **Step 2: File map**

In `CONTRIBUTING.md`'s map, the `v1alpha1/attach/` row gains "`tab.go` serves a browser tab the program's stream with `snapshot.go`'s catch-up; `frame.go` is the console's".

- [ ] **Step 3: Verify and commit**

Run: `make vet test`
Expected: PASS.

```bash
git add docs/reference.md CONTRIBUTING.md
git commit -m "docs: selecting, copying and the commands, in a tab and on the console (#274)

Co-Authored-By: Claude Fable 5.1 <noreply@anthropic.com>"
```
