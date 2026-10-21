package attach

import (
	"bytes"
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
