package shell

import (
	"errors"
	"os"
	"strings"
	"testing"
)

// TestEither pins the fallback between two ways of opening a pseudo-terminal:
// the second is asked only when the first fails, and both failing says why
// each did.
func TestEither(t *testing.T) {
	a, b := os.NewFile(0, "a"), os.NewFile(0, "b")
	opens := func(f *os.File, asked *int) opener {
		return func() (*os.File, *os.File, error) { *asked++; return f, f, nil }
	}
	fails := func(why string, asked *int) opener {
		return func() (*os.File, *os.File, error) { *asked++; return nil, nil, errors.New(why) }
	}

	var first, second int
	if m, _, err := either(opens(a, &first), opens(b, &second))(); err != nil || m != a || second != 0 {
		t.Errorf("first opens: master %v, err %v, second asked %d times; want a, nil, 0", m, err, second)
	}

	first, second = 0, 0
	if m, _, err := either(fails("no /dev/ptmx", &first), opens(b, &second))(); err != nil || m != b {
		t.Errorf("first fails: master %v, err %v; want b from the second", m, err)
	}

	_, _, err := either(fails("no /dev/ptmx", &first), fails("no /dev/pts/ptmx", &second))()
	if err == nil {
		t.Fatal("both fail: err = nil")
	}
	for _, want := range []string{"no pseudo-terminal on this machine", "no /dev/ptmx", "no /dev/pts/ptmx"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not say %q", err, want)
		}
	}
}
