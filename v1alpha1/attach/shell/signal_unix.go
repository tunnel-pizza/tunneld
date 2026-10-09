//go:build !windows

package shell

import (
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

// terminate asks the program to stop, and everything it started with it:
// SIGTERM to its session. startOn and ownGroup both start the program in a
// session of its own, so the session is the program and whatever it started —
// a dev server's workers, a shell's jobs — whichever process groups those
// are in.
func terminate(p *os.Process) error { return signalSession(p, syscall.SIGTERM) }

// kill ends the program's session outright, for one that did not leave when
// asked.
func kill(p *os.Process) error { return signalSession(p, syscall.SIGKILL) }

// interrupt is Ctrl-C for a program with no terminal to turn the key into a
// signal. A terminal sends SIGINT to its foreground group, and without one
// there is no foreground to name, so it goes to the session: to what the
// program is running, and to the program — which, a shell being interactive,
// takes it as the prompt coming back.
//
// The session and not the program's group: bash turns job control on over
// pipes, terminal or none, and puts each job in a group of its own.
func interrupt(p *os.Process) error { return signalSession(p, syscall.SIGINT) }

// hangup is a terminal closing, for a program that had none: SIGHUP to its
// session, which is what a pseudo-terminal's last close does to the session
// it controls.
func hangup(p *os.Process) error { return signalSession(p, syscall.SIGHUP) }

// signalSession signals every process in the program's session. Where the
// session cannot be listed, or the program does not lead one, it falls back
// to the program's group, and to the program alone if it does not lead a
// group either — a signal aimed at our own session or group would be aimed
// at us.
func signalSession(p *os.Process, sig syscall.Signal) error {
	if sid, err := unix.Getsid(p.Pid); err == nil && sid == p.Pid {
		if pids, err := session(sid); err == nil && len(pids) > 0 {
			for _, pid := range pids {
				_ = syscall.Kill(pid, sig)
			}
			return nil
		}
	}
	if pgid, err := syscall.Getpgid(p.Pid); err == nil && pgid == p.Pid {
		return syscall.Kill(-p.Pid, sig)
	}
	return p.Signal(sig)
}

// signalGroup is a terminal's key reaching its foreground: sig to process
// group pgrp, when it is a group in the program's session. Anything else —
// no group recorded yet, or one that has gone — reaches the session instead.
func signalGroup(p *os.Process, pgrp int, sig syscall.Signal) error {
	if pgrp > 0 && pgrp != syscall.Getpgrp() {
		if sid, err := unix.Getsid(pgrp); err == nil && sid == p.Pid {
			return syscall.Kill(-pgrp, sig)
		}
	}
	return signalSession(p, sig)
}
