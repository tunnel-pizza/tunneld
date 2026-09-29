package shell

import (
	"fmt"
	"os"

	"github.com/creack/pty"
)

// opener opens a pseudo-terminal pair: the master this package keeps, and the
// slave a program is given as its terminal. A field on TargetsImpl rather than
// a call, so a test can stand for a machine that has none.
type opener func() (master, slave *os.File, err error)

// altPTMX is where devpts keeps its own multiplexer. /dev/ptmx is normally a
// link to it, or a device node for the same thing; a sandbox that mounts
// devpts but builds a minimal /dev can have this without that.
const altPTMX = "/dev/pts/ptmx"

// openPTY opens a pair the usual way — creack/pty, which on Linux opens
// exactly /dev/ptmx — and, when that fails, through devpts' own multiplexer.
var openPTY = either(pty.Open, func() (*os.File, *os.File, error) { return openPTMX(altPTMX) })

// either opens with first, and with second when first fails. Both failing is
// a machine with no pseudo-terminals to give, and the error says why each
// failed.
func either(first, second opener) opener {
	return func() (*os.File, *os.File, error) {
		master, slave, err := first()
		if err == nil {
			return master, slave, nil
		}
		master, slave, altErr := second()
		if altErr == nil {
			return master, slave, nil
		}
		return nil, nil, fmt.Errorf("no pseudo-terminal on this machine: %w; %w", err, altErr)
	}
}
