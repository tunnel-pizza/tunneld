//go:build !windows

package shell

import (
	"os"
	"testing"
	"time"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// TestUnmeterTurnsOffFlowControl pins the settings a fresh pseudo-terminal
// arrives with and what unmeter leaves them as.
//
// The default matters as much as the result: if a pty ever stops arriving with
// IXON set, this becomes a no-op guarding nothing, and the first half of this
// test is what would say so.
func TestUnmeterTurnsOffFlowControl(t *testing.T) {
	master, slave, err := pty.Open()
	if err != nil {
		t.Skipf("no pseudo-terminals here: %v", err)
	}
	t.Cleanup(func() { _ = slave.Close(); _ = master.Close() })

	before := iflag(t, master)
	if before&unix.IXON == 0 {
		t.Error("a fresh pty arrived without IXON; unmeter is guarding nothing")
	}

	if err := unmeter(master); err != nil {
		t.Fatalf("unmeter: %v", err)
	}

	after := iflag(t, master)
	for _, tc := range []struct {
		name string
		bit  uint32
	}{
		{"IXON", unix.IXON},
		{"IXOFF", unix.IXOFF},
		{"IXANY", unix.IXANY},
	} {
		if after&tc.bit != 0 {
			t.Errorf("%s still set, want software flow control off", tc.name)
		}
	}

	// Everything else is left alone. Raw mode would turn a terminal into
	// something else entirely; this is meant to change one thing.
	const untouched = unix.ICRNL | unix.BRKINT
	if before&untouched != after&untouched {
		t.Errorf("input flags %#x became %#x, want only the flow-control bits changed", before, after)
	}
}

// iflag reads a terminal's input flags, which is where software flow control
// lives.
func iflag(t *testing.T, f *os.File) uint32 {
	t.Helper()
	term, err := unix.IoctlGetTermios(int(f.Fd()), getTermios)
	if err != nil {
		t.Fatalf("reading the terminal's settings: %v", err)
	}
	return uint32(term.Iflag)
}

// TestUnmeterKeepsTheScreenMoving pins the symptom rather than the flag: a
// viewer's Ctrl-S must not stop the program's output reaching anybody.
//
// Written as the shape it actually takes — a keystroke arriving at the master,
// then the program writing — because that is the order that reproduces it. The
// wait is a second, which is a long time for a byte that has nowhere to travel
// and short enough that a frozen terminal does not hold the suite up.
func TestUnmeterKeepsTheScreenMoving(t *testing.T) {
	for _, tc := range []struct {
		name    string
		unmeter bool
		want    bool // output reaches the other end
	}{
		{"with flow control, Ctrl-S freezes the screen", false, false},
		{"without it, the screen keeps moving", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			master, slave, err := pty.Open()
			if err != nil {
				t.Skipf("no pseudo-terminals here: %v", err)
			}
			t.Cleanup(func() { _ = slave.Close(); _ = master.Close() })

			if tc.unmeter {
				if err := unmeter(master); err != nil {
					t.Fatalf("unmeter: %v", err)
				}
			}

			// A viewer types Ctrl-S. It reaches the master, which is where a
			// keystroke arrives from the page.
			if _, err := master.Write([]byte{0x13}); err != nil {
				t.Fatalf("sending Ctrl-S: %v", err)
			}
			// Long enough for the line discipline to have acted on it, so a
			// pass cannot come from having raced ahead of it.
			time.Sleep(50 * time.Millisecond)

			go func() { _, _ = slave.Write([]byte("tick\n")) }()

			read := make(chan int, 1)
			go func() {
				buf := make([]byte, 64)
				n, _ := master.Read(buf)
				read <- n
			}()

			select {
			case n := <-read:
				if !tc.want {
					t.Errorf("%d bytes arrived, want the screen frozen — the fixture no longer reproduces what unmeter fixes", n)
				}
			case <-time.After(time.Second):
				if tc.want {
					t.Error("nothing arrived in a second; Ctrl-S still freezes the screen")
				}
			}
		})
	}
}
