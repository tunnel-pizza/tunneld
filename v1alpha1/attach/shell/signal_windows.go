//go:build windows

package shell

import (
	"errors"
	"os"
)

// terminate ends the program. Windows has no SIGTERM and no process groups
// of the Unix kind to signal, so asking and killing are the same call.
func terminate(p *os.Process) error { return p.Kill() }

// kill is terminate: there is nothing gentler to have tried first.
func kill(p *os.Process) error { return p.Kill() }

// interrupt cannot be delivered. Windows turns Ctrl-C into an event on a
// console, and a program served over pipes has none; killing it instead
// would end more than the key asked for.
func interrupt(*os.Process) error { return errors.ErrUnsupported }

// hangup has no session to hang up; closing the program's pipes is all there
// is.
func hangup(*os.Process) error { return errors.ErrUnsupported }
