package builtin

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// builtinShell runs the built-in shell the way the init hook does, in dir with
// PATH as given (unset when empty), reading script from stdin, and answers
// what it wrote to stdout — its prompts go to stderr.
func builtinShell(t *testing.T, dir, path, script string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("the built-in commands are symlinks, which Windows keeps for administrators")
	}
	cmd := exec.Command(os.Args[0], Arg)
	cmd.Dir = dir
	cmd.Stdin = strings.NewReader(script)
	env := []string{"HOME=" + t.TempDir()}
	if path != "" {
		env = append(env, "PATH="+path)
	}
	cmd.Env = env
	var errw strings.Builder
	cmd.Stderr = &errw
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("the built-in shell = %v; it wrote %q and %q", err, out, errw.String())
	}
	return string(out)
}

// TestCommands pins what the built-in shell brings: on a machine with no
// $PATH at all, ls, cat and the rest still run — this executable under their
// names — and the directory that held them is gone once the shell is.
func TestCommands(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "hello.txt"), []byte("hello from cat\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out := builtinShell(t, dir, "", "ls\ncat hello.txt\nmkdir sub\ntouch sub/new\nls sub\ncat hello.txt | grep -c hello\necho $E:"+commandsEnv+"\n")

	for _, want := range []string{"hello.txt", "hello from cat", "new", "\n1\n"} {
		if !strings.Contains(out, want) {
			t.Errorf("the shell wrote %q, want %q in it", out, want)
		}
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	commandsDir := lines[len(lines)-1]
	if !strings.Contains(filepath.Base(commandsDir), "tunneld-commands-") {
		t.Fatalf("$%s = %q, want the commands' directory", commandsEnv, commandsDir)
	}
	if _, err := os.Stat(commandsDir); !os.IsNotExist(err) {
		t.Errorf("%s outlived the shell (stat = %v)", commandsDir, err)
	}
}

// TestCommandsComeLast pins the order: the built-in commands follow the
// machine's own on $PATH, so an ls the machine has is the ls that runs.
func TestCommandsComeLast(t *testing.T) {
	bin := t.TempDir()
	ls := filepath.Join(bin, "ls")
	if err := os.WriteFile(ls, []byte("#!/bin/sh\necho the-machines-own-ls\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	out := builtinShell(t, t.TempDir(), bin, "ls\n")
	if !strings.Contains(out, "the-machines-own-ls") {
		t.Errorf("the shell wrote %q, want the machine's own ls to have run", out)
	}
}

// TestCommandNeedsTheShell pins the guard: this executable run under a
// command's name is that command only beneath the built-in shell, which
// names the commands' directory in the environment; anywhere else the name
// means nothing.
func TestCommandNeedsTheShell(t *testing.T) {
	t.Setenv(commandsEnv, "")
	args := os.Args
	t.Cleanup(func() { os.Args = args })

	os.Args = []string{"/somewhere/ls"}
	if _, ok := command(); ok {
		t.Errorf("command() for ls with no %s = ok, want the name to mean nothing", commandsEnv)
	}
	t.Setenv(commandsEnv, t.TempDir())
	if _, ok := command(); !ok {
		t.Errorf("command() for ls beneath the shell = not ok, want ls")
	}
	os.Args = []string{"/somewhere/tunneld"}
	if _, ok := command(); ok {
		t.Errorf("command() for tunneld = ok, want no command by that name")
	}
}
