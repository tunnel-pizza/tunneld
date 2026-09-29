//go:build !linux

package shell

import (
	"errors"
	"os"
)

// openPTMX has nowhere else to look. /dev/pts/ptmx is devpts', which is
// Linux's; elsewhere creack/pty's own answer is the only one.
func openPTMX(string) (*os.File, *os.File, error) {
	return nil, nil, errors.ErrUnsupported
}
