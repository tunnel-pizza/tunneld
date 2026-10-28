package preflight

import (
	"bytes"
	"context"
	"strings"
	"testing"
)

// TestPrinterStartsALineAndEndsItLater pins the streaming: a check's name
// is written when it starts, with no newline, and its result and mark end
// the same line when it finishes.
func TestPrinterStartsALineAndEndsItLater(t *testing.T) {
	var out bytes.Buffer
	p := newPrinter(&out, "tunnel.pizza")
	p.start("Edge", "HTTP/2")
	started := out.String()
	if !strings.HasSuffix(started, "  HTTP/2"+strings.Repeat(" ", p.width-len("HTTP/2")+2)) || strings.HasSuffix(started, "\n") {
		t.Fatalf("after start: %q, want the name and no newline", started)
	}
	if !strings.Contains(started, "\nEdge  port 7844, or the relay on 443\n") {
		t.Errorf("no heading before the first check of a category: %q", started)
	}
	p.end(Check{OK: false, Detail: "context deadline exceeded", Hint: "allow TCP 7844"})
	want := started + "context deadline exceeded  ✗\n" + strings.Repeat(" ", 2+p.width+2) + "→ allow TCP 7844\n"
	if out.String() != want {
		t.Errorf("after end:\n%q\nwant\n%q", out.String(), want)
	}
}

func TestPrinterMarks(t *testing.T) {
	for _, tc := range []struct {
		check Check
		mark  string
		hint  bool
	}{
		{Check{OK: true, Detail: "fine"}, "✓", false},
		{Check{OK: true, Warn: true, Detail: "fine, but", Hint: "know this"}, "!", true},
		{Check{OK: false, Detail: "broken", Hint: "ask for this"}, "✗", true},
		{Check{OK: true, Detail: "fine", Hint: "unused"}, "✓", false},
	} {
		var out bytes.Buffer
		p := newPrinter(&out, "tunnel.pizza")
		p.start("DNS", "x")
		p.end(tc.check)
		line := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
		got := line[len(line)-1]
		if tc.hint {
			got = line[len(line)-2]
		}
		if !strings.HasSuffix(got, tc.check.Detail+"  "+tc.mark) {
			t.Errorf("line %q does not end with %q", got, tc.check.Detail+"  "+tc.mark)
		}
		if has := strings.Contains(out.String(), "→"); has != tc.hint {
			t.Errorf("%+v: hint printed = %v, want %v", tc.check, has, tc.hint)
		}
	}
}

// TestRunPrintsWhatWritePrints pins that the report printed as it goes is the
// report: Run's output and Close match Write on the finished report.
func TestRunPrintsWhatWritePrints(t *testing.T) {
	server := provider(t, ok)
	p := local(t, server, edgeTCP(t, server))
	var streamed, written bytes.Buffer
	r := p.Run(context.Background(), &streamed)
	streamErr := r.Close(&streamed)
	writeErr := r.Write(&written)
	if streamed.String() != written.String() {
		t.Errorf("streamed:\n%s\nwritten:\n%s", streamed.String(), written.String())
	}
	if (streamErr == nil) != (writeErr == nil) {
		t.Errorf("Close = %v, Write = %v", streamErr, writeErr)
	}
	if !strings.HasPrefix(streamed.String(), "tunneld preflight  ") {
		t.Errorf("no title: %q", streamed.String()[:40])
	}
}

// TestNoColorOffATerminal pins that a buffer, like a pipe or a file, gets no
// escapes.
func TestNoColorOffATerminal(t *testing.T) {
	var out bytes.Buffer
	if colors(&out).on {
		t.Error("color on for a buffer")
	}
}
