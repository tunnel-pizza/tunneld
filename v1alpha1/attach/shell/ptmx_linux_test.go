package shell

import (
	"errors"
	"io"
	"io/fs"
	"strings"
	"testing"
	"time"
)

// TestOpenPTMX pins the second way of opening a pseudo-terminal: through a
// multiplexer by path, to a pair that works — what the slave is written, the
// master reads. /dev/ptmx is the same multiplexer devpts keeps at
// /dev/pts/ptmx, so it stands in for it here, where the latter is often
// root's alone.
func TestOpenPTMX(t *testing.T) {
	master, slave, err := openPTMX("/dev/ptmx")
	if err != nil {
		t.Skipf("no pseudo-terminals here: %v", err)
	}
	defer func() { _ = master.Close() }()
	defer func() { _ = slave.Close() }()

	if _, err := io.WriteString(slave, "through-the-pair\n"); err != nil {
		t.Fatalf("writing the slave: %v", err)
	}
	// Read until the line's CR LF: the master may hand the line and what
	// output processing added to it over in separate reads.
	got := make(chan string, 1)
	go func() {
		var read []byte
		b := make([]byte, 64)
		for !strings.HasSuffix(string(read), "\n") {
			n, err := master.Read(b)
			read = append(read, b[:n]...)
			if err != nil {
				break
			}
		}
		got <- string(read)
	}()
	select {
	case s := <-got:
		if s != "through-the-pair\r\n" {
			t.Errorf("master read %q, want the slave's line, as a terminal ends it", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the master read nothing")
	}

	if _, _, err := openPTMX("/dev/pts/no-such-multiplexer"); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("openPTMX(a path that is not there) = %v, want ErrNotExist", err)
	}
}
