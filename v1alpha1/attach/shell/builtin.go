package shell

import (
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	v1 "github.com/tunnel-pizza/tunneld/v1"
	"src.elv.sh/pkg/prog"
	elvish "src.elv.sh/pkg/shell"
)

// BuiltinArg is what a process is started with to be the shell built into
// tunneld rather than tunneld: its one argument, and nothing else. Spelled so
// nothing a person types at tunneld collides with it.
const BuiltinArg = "--tunneld-builtin-shell"

// init turns a process started with BuiltinArg into the built-in shell before
// anything else in it runs, and never returns.
//
// An init rather than a branch in main, because the executable is whichever
// program links tunneld — tunneld's own, or one that embeds it under a verb of
// its own — and an embedding program's main knows nothing about this. Any
// binary that links this package answers BuiltinArg the same way, and nothing
// else changes for it: the check is one comparison of its arguments.
func init() {
	if len(os.Args) == 2 && os.Args[1] == BuiltinArg {
		os.Exit(Run())
	}
}

// Builtin is the origin of the shell built into tunneld: this executable, run
// with BuiltinArg, as an ordinary program origin. So it is served exactly the
// way any program is — on a pseudo-terminal that is its controlling terminal,
// which is what makes Ctrl-C reach what it runs — and the only thing that
// knows it is not a program on disk is init above.
func Builtin() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("find this executable to run its built-in shell: %w", err)
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	u := url.URL{Scheme: v1.ExecScheme, Path: exe, RawQuery: url.Values{v1.ArgKey: {BuiltinArg}}.Encode()}
	return u.String(), nil
}

// Run is the built-in shell: Elvish (https://elv.sh), interactive on this
// process's own terminal, until it exits; the result is its exit status.
//
// Elvish rather than a POSIX interpreter because what it is for is a person at
// a terminal: it brings its own line editor — history, completion, the arrow
// keys — where a POSIX interpreter in Go has none. Its language is its own,
// which is the trade: `export`, `$(...)` and `&&` are spelled differently.
//
// Without Elvish's storage daemon, which is a second process and a database
// under the user's state directory: history lives as long as the session,
// and nothing is left behind on a machine that only ever had a shell because
// it had none. Elvish's own rc file is read if there is one, as any shell's is.
func Run() int {
	return prog.Run([3]*os.File{os.Stdin, os.Stdout, os.Stderr}, []string{"elvish"}, &elvish.Program{})
}
