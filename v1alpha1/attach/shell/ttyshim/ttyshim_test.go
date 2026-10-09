package ttyshim

import (
	"bytes"
	"debug/elf"
	"runtime"
	"testing"
)

// TestObject pins that a Linux amd64/arm64 build carries a shared object for
// its own architecture, and every other build carries nothing.
func TestObject(t *testing.T) {
	supported := runtime.GOOS == "linux" && (runtime.GOARCH == "amd64" || runtime.GOARCH == "arm64")
	if !supported {
		if Object() != nil || Terminfo() != nil {
			t.Fatal("a build with no shim carries one")
		}
		return
	}
	f, err := elf.NewFile(bytes.NewReader(Object()))
	if err != nil {
		t.Fatalf("Object() is not ELF: %v", err)
	}
	want := map[string]elf.Machine{"amd64": elf.EM_X86_64, "arm64": elf.EM_AARCH64}[runtime.GOARCH]
	if f.Type != elf.ET_DYN || f.Machine != want {
		t.Errorf("Object() is %v for %v, want ET_DYN for %v", f.Type, f.Machine, want)
	}
	if len(Terminfo()) < 1000 {
		t.Errorf("Terminfo() is %d bytes; want the compiled entry", len(Terminfo()))
	}
}
