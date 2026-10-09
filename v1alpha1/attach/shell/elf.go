package shell

import (
	"bufio"
	"debug/elf"
	"os"
	"runtime"
	"strings"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell/ttyshim"
)

// shimReason is why the program at path cannot be given the terminal shim,
// or "" when it can. The shim lives in libc, so it reaches only programs the
// dynamic loader starts and that ask libc about their terminal: not a
// statically linked one, and not a Go one, which asks the kernel itself. A
// script is judged by its interpreter, followed once.
func shimReason(path string) string {
	if ttyshim.Object() == nil {
		return "no terminal shim for " + runtime.GOOS + "/" + runtime.GOARCH
	}
	return elfReason(path, true)
}

func elfReason(path string, follow bool) string {
	f, err := os.Open(path)
	if err != nil {
		return "not an ELF program"
	}
	defer func() { _ = f.Close() }()
	head := make([]byte, 2)
	if _, err := f.ReadAt(head, 0); err == nil && string(head) == "#!" && follow {
		line, _ := bufio.NewReader(f).ReadString('\n')
		fields := strings.Fields(strings.TrimPrefix(line, "#!"))
		if len(fields) == 0 {
			return "not an ELF program"
		}
		return elfReason(fields[0], false)
	}
	ef, err := elf.NewFile(f)
	if err != nil {
		return "not an ELF program"
	}
	if ef.Section(".go.buildinfo") != nil {
		return "a Go program"
	}
	for _, p := range ef.Progs {
		if p.Type == elf.PT_INTERP {
			return ""
		}
	}
	return "statically linked"
}
