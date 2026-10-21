package attach

import (
	"bytes"
	"strings"
	"sync"
	"testing"
)

// TestMirrorWritesWholeLinesBehindThePrefix pins what a run's own terminal
// gets of a program's output: each line once it ends, behind the prefix,
// however the program's writes split it; the carriage return a terminal adds
// before the newline taken off; and a last line with no newline written once
// the run is over.
func TestMirrorWritesWholeLinesBehindThePrefix(t *testing.T) {
	var out bytes.Buffer
	m := &mirror{mu: &sync.Mutex{}, w: &out, prefix: "[next] "}
	for _, p := range []string{"ready on ", "http://localhost:3000\r\n", "compiled\r\n", "> "} {
		_, _ = m.Write([]byte(p))
	}
	if want := "[next] ready on http://localhost:3000\n[next] compiled\n"; out.String() != want {
		t.Errorf("mirrored %q, want %q", out.String(), want)
	}
	m.flush()
	if want := "[next] ready on http://localhost:3000\n[next] compiled\n[next] > \n"; out.String() != want {
		t.Errorf("after flush %q, want %q", out.String(), want)
	}
}

// TestMirrorHoldsNoLineWithoutEnd pins the bound on what a mirror holds: a
// program writing on without a newline has its output written in pieces of
// mirrorLine, each behind the prefix, rather than held until it stops.
func TestMirrorHoldsNoLineWithoutEnd(t *testing.T) {
	var out bytes.Buffer
	m := &mirror{mu: &sync.Mutex{}, w: &out, prefix: "[bar] "}
	_, _ = m.Write(bytes.Repeat([]byte("#"), mirrorLine*2+10))
	if got, want := strings.Count(out.String(), "[bar] "), 2; got != want {
		t.Errorf("%d pieces written, want %d", got, want)
	}
	if len(m.line) != 10 {
		t.Errorf("holding %d bytes, want the 10 past the last piece", len(m.line))
	}
}
