package shell

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/u-root/u-root/pkg/core"
	"github.com/u-root/u-root/pkg/core/base64"
	"github.com/u-root/u-root/pkg/core/cat"
	"github.com/u-root/u-root/pkg/core/chmod"
	"github.com/u-root/u-root/pkg/core/cp"
	"github.com/u-root/u-root/pkg/core/find"
	"github.com/u-root/u-root/pkg/core/gzip"
	"github.com/u-root/u-root/pkg/core/ls"
	"github.com/u-root/u-root/pkg/core/mkdir"
	"github.com/u-root/u-root/pkg/core/mktemp"
	"github.com/u-root/u-root/pkg/core/mv"
	"github.com/u-root/u-root/pkg/core/rm"
	"github.com/u-root/u-root/pkg/core/shasum"
	"github.com/u-root/u-root/pkg/core/tar"
	"github.com/u-root/u-root/pkg/core/touch"
	"github.com/u-root/u-root/pkg/core/xargs"
)

// commands are the programs the built-in shell brings with it, for a machine
// that has none: u-root's Go implementations of the core Unix commands
// (github.com/u-root/u-root/pkg/core), by the name each is run as. gzip is
// three names, told apart by the one it is given.
var commands = map[string]func() core.Command{
	"base64": base64.New,
	"cat":    cat.New,
	"chmod":  chmod.New,
	"cp":     cp.New,
	"find":   find.New,
	"gunzip": func() core.Command { return gzip.New("gunzip") },
	"gzcat":  func() core.Command { return gzip.New("gzcat") },
	"gzip":   func() core.Command { return gzip.New("gzip") },
	"ls":     ls.New,
	"mkdir":  mkdir.New,
	"mktemp": mktemp.New,
	"mv":     mv.New,
	"rm":     rm.New,
	"shasum": shasum.New,
	"tar":    tar.New,
	"touch":  touch.New,
	"xargs":  xargs.New,
}

// commandsEnv names the directory of the built-in shell's commands, in the
// environment of the shell and everything it starts. It is what lets this
// executable, run under one of those names, be that command: a program that
// merely happens to be called ls and links tunneld is not given the name's
// meaning by accident.
const commandsEnv = "TUNNELD_BUILTIN_COMMANDS"

// command is the command this process was started as, when it was started as
// one of the built-in shell's: its name is one of commands', and it runs
// under a shell that put them on its $PATH.
func command() (func() core.Command, bool) {
	if os.Getenv(commandsEnv) == "" {
		return nil, false
	}
	name := strings.TrimSuffix(filepath.Base(os.Args[0]), ".exe")
	newCommand, ok := commands[name]
	return newCommand, ok
}

// runCommand runs a built-in command with this process's arguments, streams,
// working directory and environment, and answers its exit status.
func runCommand(newCommand func() core.Command) int {
	c := newCommand()
	c.SetIO(os.Stdin, os.Stdout, os.Stderr)
	if wd, err := os.Getwd(); err == nil {
		c.SetWorkingDir(wd)
	}
	c.SetLookupEnv(os.LookupEnv)
	if err := c.RunContext(context.Background(), os.Args[1:]...); err != nil {
		fmt.Fprintf(os.Stderr, "%s: %v\n", filepath.Base(os.Args[0]), err)
		return 1
	}
	return 0
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
	names := make([]string, 0, len(commands))
	for name := range commands {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		if err := os.Symlink(exe, filepath.Join(dir, name)); err != nil {
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
