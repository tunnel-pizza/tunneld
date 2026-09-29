package shell

import (
	"errors"
	"os"
	"os/exec"
)

// startOn has no terminal to start a program on: Windows has no pseudo-
// terminals for this package to open, so a target there is served over pipes
// and never reaches this.
func startOn(*exec.Cmd, *os.File) error { return errors.ErrUnsupported }

// ownGroup has no group to make. Windows has no process groups of the Unix
// kind to signal, which is also why an interrupt there does nothing.
func ownGroup(*exec.Cmd) {}
