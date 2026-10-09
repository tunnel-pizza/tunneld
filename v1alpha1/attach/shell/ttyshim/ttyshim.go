// Package ttyshim carries the terminal shim tunneld preloads into a program it
// serves on a Linux machine with no pseudo-terminals, and the terminfo entry
// for the TERM it sets. The objects are built by build.sh in Docker and
// committed, so tunneld builds as pure Go.
package ttyshim

// Object is this architecture's shim, or nil where there is none.
func Object() []byte { return object }

// Terminfo is the compiled xterm-256color entry, or nil where there is no
// shim to use it.
func Terminfo() []byte { return terminfo }
