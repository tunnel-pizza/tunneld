package shell

import "os"

// unmeter has nothing to turn off. Windows has no pseudo-terminals for this
// package to create, so a program there is served over pipes and nothing
// reaches this — see Open's pty probe. The file exists so that the caller
// reads the same on every platform.
func unmeter(*os.File) error { return nil }
