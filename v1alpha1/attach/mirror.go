package attach

import (
	"bytes"
	"io"
	"sync"
)

// mirror copies a program's output to a run's own terminal a line at a time,
// each line behind the program's prefix: what docker compose up does with its
// services' output. The lock is shared by every mirror writing to w, so the
// lines of two programs never interleave mid-line.
//
// What it gets is what the terminal draws, OSC and private modes already
// taken out, so a line-oriented program (a dev server, a test suite) reads
// well and a full-screen one does not.
type mirror struct {
	mu     *sync.Mutex
	w      io.Writer
	prefix string
	line   []byte
}

func (m *mirror) Write(p []byte) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := len(p)
	for {
		i := bytes.IndexByte(p, '\n')
		if i < 0 {
			m.line = append(m.line, p...)
			return n, nil
		}
		m.line = append(m.line, p[:i]...)
		m.emit()
		p = p[i+1:]
	}
}

// flush writes a line the program left unfinished, once its run is over.
func (m *mirror) flush() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.line) > 0 {
		m.emit()
	}
}

// emit writes the line held, behind the prefix. The caller holds mu.
func (m *mirror) emit() {
	line := bytes.TrimRight(m.line, "\r")
	out := make([]byte, 0, len(m.prefix)+len(line)+1)
	out = append(append(append(out, m.prefix...), line...), '\n')
	_, _ = m.w.Write(out)
	m.line = m.line[:0]
}
