package shell

import (
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/tunnel-pizza/tunneld/v1alpha1/attach/shell/ttyshim"
)

// TestShimReason pins which programs get the shim: a dynamically linked
// program and a script run by one do; a Go program, a static one, something
// that is not a program, and a platform with no shim do not.
func TestShimReason(t *testing.T) {
	if ttyshim.Object() == nil {
		if got := shimReason("/bin/sh"); got == "" {
			t.Fatal("shimReason says yes where there is no shim")
		}
		return
	}
	dir := t.TempDir()
	self, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	script := func(name, interp string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte("#!"+interp+" -e\necho\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	text := filepath.Join(dir, "notes")
	_ = os.WriteFile(text, []byte("hello"), 0o644)

	for _, tc := range []struct {
		name, path, want string
	}{
		{"a dynamic program", "/bin/sh", ""},
		{"a script run by one", script("dyn", "/bin/sh"), ""},
		{"a Go program", self, "a Go program"},
		{"a script run by a Go program", script("go", self), "a Go program"},
		{"a script run by a Go program through env", envScript(t, dir, self), "a Go program"},
		{"not a program", text, "not an ELF program"},
		{"a script whose interpreter is missing", script("gone", "/nonexistent/sh"), "not an ELF program"},
	} {
		if got := shimReason(tc.path); got != tc.want {
			t.Errorf("%s: shimReason(%q) = %q, want %q", tc.name, tc.path, got, tc.want)
		}
	}
	if runtime.GOOS == "linux" {
		if bb, err := exec.LookPath("busybox.static"); err == nil {
			if got := shimReason(bb); got != "statically linked" {
				t.Errorf("shimReason(busybox.static) = %q, want statically linked", got)
			}
		}
	}
}

// envScript is a script whose #! names env, which runs prog.
func envScript(t *testing.T, dir, prog string) string {
	p := filepath.Join(dir, "via-env")
	if err := os.WriteFile(p, []byte("#!/usr/bin/env "+prog+"\necho\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}
