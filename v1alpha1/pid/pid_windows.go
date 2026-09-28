package pid

import (
	"errors"
	"os"
)

// The npm launcher does not detach on Windows, so nothing there sets
// v1.NotifyPidEnv and these are never reached; they keep the build whole.

func redirect(*os.File) error { return errors.ErrUnsupported }

func notify(int) error { return errors.ErrUnsupported }
