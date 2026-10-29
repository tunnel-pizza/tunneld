//go:build unix

package pid

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

// redirect points this process's stdout and stderr at f. os.Stdout and
// os.Stderr keep their descriptors, so everything that writes through them —
// the logger, cobra — carries on into f.
func redirect(f *os.File) error {
	for _, fd := range []int{int(os.Stdout.Fd()), int(os.Stderr.Fd())} {
		if err := unix.Dup2(int(f.Fd()), fd); err != nil {
			return err
		}
	}
	return nil
}

// notify tells the launcher this run is up. SIGUSR2 rather than SIGUSR1,
// which Node keeps for its debugger.
func notify(pid int) error {
	return unix.Kill(pid, unix.SIGUSR2)
}

// pipe is fd as a file when it is an open pipe or socket — a Node launcher's
// stdio pipe is a socketpair — made close-on-exec, or nil.
func pipe(fd int) *os.File {
	var st unix.Stat_t
	if unix.Fstat(fd, &st) != nil {
		return nil
	}
	if kind := st.Mode & unix.S_IFMT; kind != unix.S_IFIFO && kind != unix.S_IFSOCK {
		return nil
	}
	unix.CloseOnExec(fd)
	return os.NewFile(uintptr(fd), "pid")
}

// lock takes an exclusive lock on f without waiting, or returns errLocked when
// another open file holds it. flock's lock belongs to the open file, so the
// kernel drops it as the file is closed, however the process ends.
func lock(f *os.File) error {
	err := unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
	if errors.Is(err, unix.EWOULDBLOCK) {
		return errLocked
	}
	return err
}
