package motd

import (
	"bytes"
	"io"
	"log/slog"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/x/ansi"
	"github.com/creack/pty"
)

var sgr = regexp.MustCompile(`\x1b\[[0-9;]*m`)

// osc strips an operating system command, such as an OSC 8 hyperlink, for
// comparing what a row says rather than what it links to.
var osc = regexp.MustCompile(`\x1b\][^\x1b\x07]*(\x1b\\|\x07)`)

func learned(t *testing.T, markdown ...string) *MotdImpl {
	t.Helper()
	m := New()
	raw := make([]string, len(markdown))
	for i, md := range markdown {
		raw[i] = dataURL(md)
	}
	m.Learn(raw, slog.New(slog.DiscardHandler))
	return m
}

// TestPrint pins the stderr rendering: a label per message, the body rendered
// after it, nothing at all when there is nothing to say.
func TestPrint(t *testing.T) {
	var out bytes.Buffer
	New().Print(&out, 80)
	if out.Len() != 0 {
		t.Errorf("Print with nothing learned wrote %q", out.String())
	}

	// A bytes.Buffer is never a terminal, so Print must fall back to plain
	// text: no SGR anywhere, and the body as decoded rather than through
	// glamour (the asterisks survive; glamour would have styled them away).
	// That also means colour can't be asserted from this call — the
	// terminal path is exercised live, per the design doc.
	m := learned(t, "> [!warning]\n> This tunnel is **public**.", "> [!note]\n> Expires soon.")
	out.Reset()
	m.Print(&out, 80)
	got := out.String()
	if strings.Contains(got, "\x1b[") {
		t.Errorf("Print wrote %q, want no SGR off a terminal", got)
	}
	for _, want := range []string{"WARNING", "This tunnel is **public**.", "NOTE", "Expires soon."} {
		if !strings.Contains(got, want) {
			t.Errorf("Print wrote %q, want it to contain %q", got, want)
		}
	}
	if strings.Index(got, "WARNING") > strings.Index(got, "NOTE") {
		t.Error("messages printed out of order")
	}
}

// TestPrintOnATerminal pins the path TestPrint cannot reach: on a real tty
// the label is coloured and the body goes through glamour, so the emphasis
// markers are gone from what the operator reads.
func TestPrintOnATerminal(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("no pty on Windows")
	}
	ptmx, tty, err := pty.Open()
	if err != nil {
		t.Skipf("no pty to be a terminal on: %v", err)
	}
	t.Cleanup(func() { tty.Close(); ptmx.Close() })

	// The master is read on its own goroutine: a pty's buffer is small, and
	// Print would block on a full one if nothing drained it. Reading stops at
	// a marker written after Print rather than at the tty's close, since a
	// closing tty may discard what the master has not read yet.
	const done = "--done--"
	read := make(chan string, 1)
	go func() {
		var b strings.Builder
		buf := make([]byte, 4096)
		for !strings.Contains(b.String(), done) {
			n, err := ptmx.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
		}
		read <- b.String()
	}()
	learned(t, "> [!warning]\n> Be **careful**.").Print(tty, 60)
	_, _ = io.WriteString(tty, done+"\n")

	var got string
	select {
	case got = <-read:
	case <-time.After(5 * time.Second):
		t.Fatal("nothing came back from the pty")
	}
	at := strings.Index(got, "WARNING")
	if at < 0 {
		t.Fatalf("Print wrote %q, want the label", got)
	}
	if !strings.Contains(got[:at], "\x1b[") {
		t.Errorf("Print wrote %q, want an SGR sequence before the label", got)
	}
	if !strings.Contains(got, "careful") || strings.Contains(got, "**") {
		t.Errorf("Print wrote %q, want the emphasis rendered by glamour", got)
	}
}

// TestLines pins the frame's row per message: label and body on one line,
// newlines folded, truncated to the width with an ellipsis, and a message that
// is only its alert line still gets a row.
func TestLines(t *testing.T) {
	m := learned(t, "> [!caution]\n> line one\n> line two", "> [!warning]", "no alert here")
	lines := m.Lines(40)
	if len(lines) != 3 {
		t.Fatalf("Lines() gave %d rows, want one per message", len(lines))
	}
	plain := make([]string, len(lines))
	for i, l := range lines {
		plain[i] = sgr.ReplaceAllString(l, "")
	}
	if plain[0] != "CAUTION line one line two" {
		t.Errorf("row 0 = %q, want label and the body on one line", plain[0])
	}
	if plain[1] != "WARNING" {
		t.Errorf("row 1 = %q, want the bare label", plain[1])
	}
	if plain[2] != "no alert here" {
		t.Errorf("row 2 = %q, want the body with no label", plain[2])
	}
	if !strings.Contains(lines[0], "\x1b[") {
		t.Error("a caution row carries no colour")
	}
	// A row is the strip's own bar: the severity fills the background and the
	// text is the contrasting colour, not just coloured foreground text.
	if !strings.Contains(lines[0], "48;5;196") || !strings.Contains(lines[0], "38;5;255") {
		t.Errorf("caution row = %q, want a 196 background and 255 (light) text", lines[0])
	}
	if !strings.Contains(lines[1], "48;5;214") || !strings.Contains(lines[1], "38;5;232") {
		t.Errorf("warning row = %q, want a 214 background and 232 (dark) text", lines[1])
	}

	long := learned(t, "> [!note]\n> "+strings.Repeat("x", 100))
	row := sgr.ReplaceAllString(long.Lines(20)[0], "")
	if w := ansi.StringWidth(row); w != 20 || !strings.HasSuffix(row, "…") {
		t.Errorf("truncated row = %q (width %d), want width 20 ending in …", row, w)
	}

	wide := learned(t, "> [!note]\n> 日本語のメッセージです")
	row = sgr.ReplaceAllString(wide.Lines(12)[0], "")
	if w := ansi.StringWidth(row); w > 12 {
		t.Errorf("wide row = %q has width %d, want at most 12", row, w)
	}

	linked := learned(t, "> [!warning]\n> This tunnel is **public**. [manage](https://tunnel.pizza/cnuss?utm_source=x)")
	raw := linked.Lines(80)[0]
	if got := osc.ReplaceAllString(sgr.ReplaceAllString(raw, ""), ""); got != "WARNING This tunnel is public. manage" {
		t.Errorf("row = %q, want the message's text with the link as its text", got)
	}
	if want := ansi.SetHyperlink("https://tunnel.pizza/cnuss?utm_source=x") + "manage" + ansi.ResetHyperlink(); !strings.Contains(raw, want) {
		t.Errorf("row = %q, want the link's text wrapped in an OSC 8 hyperlink to its destination", raw)
	}
	// Cut inside the link's text: the row still closes the link, or it
	// would run on into whatever is drawn after the row. ansi.Truncate keeps
	// the sequences past the cut, which is what this pins.
	cut := sgr.ReplaceAllString(linked.Lines(33)[0], "")
	if !strings.HasSuffix(cut, ansi.ResetHyperlink()) {
		t.Errorf("truncated row = %q, want it to end by closing the hyperlink", cut)
	}
	spaced := learned(t, "> [!note]\n> see [docs](<https://x.test/a b>)")
	if row := spaced.Lines(80)[0]; strings.Contains(row, "\x1b]8;") {
		t.Errorf("row = %q, want no hyperlink for a destination with whitespace in it", row)
	}

	if got := New().Lines(40); got != nil {
		t.Errorf("Lines with nothing learned = %v, want nil", got)
	}
}

// TestContrast pins which text colour reads on each severity's fill: near-black
// on the two light hues, near-white on the dark one, nothing for no severity.
func TestContrast(t *testing.T) {
	for _, tc := range []struct {
		severity Severity
		want     ansi.Color
	}{
		{SeverityNote, ansi.IndexedColor(232)},
		{SeverityWarning, ansi.IndexedColor(232)},
		{SeverityCaution, ansi.IndexedColor(255)},
		{Severity(""), nil},
	} {
		if got := tc.severity.Contrast(); got != tc.want {
			t.Errorf("Severity(%q).Contrast() = %v, want %v", tc.severity, got, tc.want)
		}
	}
}

// TestHTML pins the panel's rendering: markdown becomes HTML, raw HTML in a
// message is escaped rather than served, links open in a new tab, and a
// non-markdown message is escaped text.
func TestHTML(t *testing.T) {
	m := learned(t, "> [!warning]\n> Be **careful** <script>alert(1)</script> and see [docs](https://tunnel.pizza/docs).")
	r := m.HTML()
	if len(r) != 1 || r[0].Severity != SeverityWarning {
		t.Fatalf("HTML() = %+v, want one warning", r)
	}
	if r[0].Label != "WARNING" {
		t.Errorf("Label = %q, want the severity as its heading, WARNING", r[0].Label)
	}
	h := string(r[0].HTML)
	for _, want := range []string{"<strong>careful</strong>", "&lt;script&gt;", `href="https://tunnel.pizza/docs"`, `target="_blank"`, `rel="noopener"`} {
		if !strings.Contains(h, want) {
			t.Errorf("HTML = %q, want it to contain %q", h, want)
		}
	}
	if strings.Contains(h, "<script>") {
		t.Errorf("HTML = %q served raw HTML from a message", h)
	}

	plain := New()
	plain.Learn([]string{"data:text/plain,<b>not markdown</b>"}, slog.New(slog.DiscardHandler))
	if got := string(plain.HTML()[0].HTML); got != "<p>&lt;b&gt;not markdown&lt;/b&gt;</p>" {
		t.Errorf("text/plain HTML = %q, want it escaped in a paragraph", got)
	}
	if got := New().HTML(); got != nil {
		t.Errorf("HTML with nothing learned = %v, want nil", got)
	}
	// A message that is only its alert line: one entry, an empty body, no panic.
	if got := learned(t, "> [!caution]").HTML(); len(got) != 1 || got[0].Severity != SeverityCaution || got[0].HTML != "" {
		t.Errorf("alert-only HTML = %+v, want one caution with an empty body", got)
	}
}
