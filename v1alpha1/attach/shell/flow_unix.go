//go:build !windows

package shell

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// unmeter turns software flow control off on a pseudo-terminal.
//
// A pty is created with the system's default line settings, and those have
// IXON set, so 0x13 never reaches the program: the line discipline eats it and
// stops the program's writes. On a shared terminal that is one viewer freezing
// the screen for every other one, with the session perfectly healthy behind it
// and Ctrl-Q — which most people do not know — the only way back.
//
// The mechanism is inert in its original purpose here. Software flow control
// exists to stop a sender overrunning a serial line, and the path between the
// program and a viewer is a websocket over a tunnel, with a pipe and an
// emulator in the middle, every one of which buffers or blocks on its own.
// What is left is the side effect.
//
// IXOFF and IXANY go with it, as the other halves of the same idea: the system
// sending XOFF upstream, and any key restarting a stopped screen. All three
// gone means Ctrl-S and Ctrl-Q are bytes like any others, which is what a
// program that binds them expects.
//
// Set on the master rather than the slave. It is one line discipline either
// way, and the master is the end this package keeps.
func unmeter(pty *os.File) error {
	fd := int(pty.Fd())
	term, err := unix.IoctlGetTermios(fd, getTermios)
	if err != nil {
		return fmt.Errorf("reading the terminal's settings: %w", err)
	}
	term.Iflag &^= unix.IXON | unix.IXOFF | unix.IXANY
	if err := unix.IoctlSetTermios(fd, setTermios, term); err != nil {
		return fmt.Errorf("writing the terminal's settings: %w", err)
	}
	return nil
}
