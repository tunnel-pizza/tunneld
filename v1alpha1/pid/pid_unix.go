//go:build unix

package pid

import (
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
