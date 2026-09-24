//go:build !windows

package shell

import (
	"os"
	"syscall"
)

// terminate asks the program to stop, and everything it started with it:
// SIGTERM to its process group. pty.Start puts the program in a session of
// its own, so its group id is its pid and its workers are in the group. If
// it somehow does not lead a group of its own, only the process is signalled
// — a group signal aimed at our own group would be aimed at us.
func terminate(p *os.Process) error { return signalGroup(p, syscall.SIGTERM) }

// kill ends the program's group outright, for one that did not leave when
// asked.
func kill(p *os.Process) error { return signalGroup(p, syscall.SIGKILL) }

func signalGroup(p *os.Process, sig syscall.Signal) error {
	if pgid, err := syscall.Getpgid(p.Pid); err == nil && pgid == p.Pid {
		return syscall.Kill(-p.Pid, sig)
	}
	return p.Signal(sig)
}
