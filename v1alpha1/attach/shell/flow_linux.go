package shell

import "golang.org/x/sys/unix"

// The ioctls that read and write a terminal's settings. Linux spells them
// TCGETS/TCSETS where the BSDs spell them TIOCGETA/TIOCSETA, which is the
// whole of the difference and the only reason this file exists.
const (
	getTermios = unix.TCGETS
	setTermios = unix.TCSETS
)
