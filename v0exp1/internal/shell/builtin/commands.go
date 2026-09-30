package builtin

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"

	"github.com/tunnel-pizza/tunneld/v0exp1/internal/shell/builtin/internal/bbmain"
)

//go:generate go run generate.go

// The commands the built-in shell brings, for a machine that has none, are
// u-root's (https://github.com/u-root/u-root): every one in github.com/u-root/u-root/cmds
// that builds for the operating system, rewritten by generate.go from a
// program into a package under internal/ that registers itself with bbmain by
// its name. zz_commands_<os>.go links each system's; bbmain.ListCmds names
// them.

// commandsEnv names the directory of the built-in shell's commands, in the
// environment of the shell and everything it starts. It is what lets this
// executable, run under one of those names, be that command: a program that
// merely happens to be called ls and links tunneld is not given the name's
// meaning by accident.
const commandsEnv = "TUNNELD_BUILTIN_COMMANDS"

// command is the command this process was started as, when it was started as
// one of the built-in shell's: its name is one of bbmain's, and it runs under a
// shell that put them on its $PATH.
func command() (string, bool) {
	if os.Getenv(commandsEnv) == "" {
		return "", false
	}
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	return name, slices.Contains(bbmain.ListCmds(), name)
}

// runCommand runs the built-in command name as u-root's own program would
// run, on this process's arguments, streams and environment. It does not
// return when the command runs; bbmain exits with the command's status.
func runCommand(name string) int {
	if err := bbmain.Run(name); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", name, err)
	}
	return 1
}

// installCommands makes a directory of links to this executable, one per
// built-in command, and puts it at the end of $PATH — after everything the
// machine has, so an ls it already has is the ls that runs, and these only
// fill what is missing. The returned function removes the directory.
//
// Links rather than copies: each is this executable under another name, and
// command() tells them apart by it.
func installCommands() (func(), error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "tunneld-commands-")
	if err != nil {
		return nil, err
	}
	remove := func() { _ = os.RemoveAll(dir) }
	ext := ""
	if runtime.GOOS == "windows" {
		ext = ".exe"
	}
	for _, name := range bbmain.ListCmds() {
		if err := os.Symlink(exe, filepath.Join(dir, name+ext)); err != nil {
			remove()
			return nil, err
		}
	}
	path := dir
	if existing := os.Getenv("PATH"); existing != "" {
		path = existing + string(os.PathListSeparator) + dir
	}
	_ = os.Setenv("PATH", path)
	_ = os.Setenv(commandsEnv, dir)
	return remove, nil
}
