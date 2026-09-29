//go:build !linux && !darwin

package shell

import "errors"

// session cannot list a session's processes here; a signal meant for one goes
// to the program's process group instead.
func session(int) ([]int, error) { return nil, errors.ErrUnsupported }
