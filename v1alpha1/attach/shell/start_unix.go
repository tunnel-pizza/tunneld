//go:build !windows

package shell

import (
	"os"
	"os/exec"
	"syscall"
)

// startOn starts cmd with slave as its terminal: all three streams, and its
// controlling terminal in a session of its own — which is what makes Ctrl-C
// reach what it runs, and its group id its pid. What pty.Start does, for a
// pair that did not come from pty.Open.
func startOn(cmd *exec.Cmd, slave *os.File) error {
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true}
	return cmd.Start()
}

// ownGroup puts a program started without a terminal in a session of its
// own, and so a process group of its own, so that an interrupt, an end and a
// kill reach it and what it started, and nothing of tunneld's.
//
// A session rather than only a group: a session has no controlling terminal
// until one is given it, and this one never is. In tunneld's session the
// program's /dev/tty would be the terminal tunneld was started from, and an
// interactive shell opens it to take job control — of somebody else's
// terminal, with its own jobs in groups the interrupt no longer reaches.
func ownGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
}
