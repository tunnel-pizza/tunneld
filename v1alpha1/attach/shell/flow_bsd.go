//go:build darwin || dragonfly || freebsd || netbsd || openbsd

package shell

import "golang.org/x/sys/unix"

// The ioctls that read and write a terminal's settings. The BSDs, macOS among
// them, spell them TIOCGETA/TIOCSETA where Linux spells them TCGETS/TCSETS.
const (
	getTermios = unix.TIOCGETA
	setTermios = unix.TIOCSETA
)
