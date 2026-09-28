package pid

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

// The npm launcher does not detach on Windows, so nothing there sets
// v1.NotifyPidEnv and these two are never reached; they keep the build whole.

func redirect(*os.File) error { return errors.ErrUnsupported }

func notify(int) error { return errors.ErrUnsupported }

// lock takes an exclusive lock on f without waiting, or returns errLocked when
// another handle holds it. The byte locked sits far past the end of the file:
// a Windows lock is mandatory, and one over the pid would stop a refused run
// from reading whose it is. Released as the handle is closed.
func lock(f *os.File) error {
	ol := &windows.Overlapped{OffsetHigh: 1}
	err := windows.LockFileEx(windows.Handle(f.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, ol)
	if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
		return errLocked
	}
	return err
}
