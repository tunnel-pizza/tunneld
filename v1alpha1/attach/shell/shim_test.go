package shell

import (
	"slices"
	"strings"
	"testing"
)

// TestShimEnv pins the environment rung 2 gives a program: the shim after any
// preload already there, the page, TERM and terminfo, and nothing of the
// same names left over from tunneld's own environment.
func TestShimEnv(t *testing.T) {
	got := shimEnv([]string{"HOME=/root", "TERM=dumb", "LD_PRELOAD=/opt/other.so", "TUNNELD_TTY=/stale"},
		"/tmp/d/ttyshim-x.so", "/tmp/d/tty-1", "/tmp/d")
	want := []string{
		"HOME=/root",
		"LD_PRELOAD=/opt/other.so:/tmp/d/ttyshim-x.so",
		"TUNNELD_TTY=/tmp/d/tty-1",
		"TUNNELD_TTY_SHIM=/tmp/d/ttyshim-x.so",
		"TERM=xterm-256color",
		"TERMINFO_DIRS=/tmp/d/terminfo:",
	}
	if !slices.Equal(got, want) {
		t.Errorf("shimEnv() =\n%q\nwant\n%q", got, want)
	}
	if bare := shimEnv(nil, "/s.so", "/p", "/d"); !slices.Contains(bare, "LD_PRELOAD=/s.so") {
		t.Errorf("with no preload of its own, shimEnv() = %q", bare)
	}
}

// TestShimName pins that the shim's file is named by its content, so two
// tunnelds of different versions never share one.
func TestShimName(t *testing.T) {
	a, b := shimName([]byte("one")), shimName([]byte("two"))
	if a == b || !strings.HasPrefix(a, "ttyshim-") || !strings.HasSuffix(a, ".so") {
		t.Errorf("shimName gave %q and %q", a, b)
	}
}
