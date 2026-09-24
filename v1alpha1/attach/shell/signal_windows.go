//go:build windows

package shell

import "os"

// terminate ends the program. Windows has no SIGTERM and no process groups
// of the Unix kind to signal, so asking and killing are the same call.
func terminate(p *os.Process) error { return p.Kill() }

// kill is terminate: there is nothing gentler to have tried first.
func kill(p *os.Process) error { return p.Kill() }
