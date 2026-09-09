package shell

import "os"

// unmeter has nothing to turn off. Windows has no pseudo-terminals for this
// package to create, so Open refuses a program origin there long before
// anything reaches this — see its pty probe. The file exists so that the
// caller reads the same on every platform.
func unmeter(*os.File) error { return nil }
