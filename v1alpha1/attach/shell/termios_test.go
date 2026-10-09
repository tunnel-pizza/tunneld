package shell

import "testing"

// TestModes pins the two fixed modes: a fresh Linux pseudo-terminal's, and
// pipes', which is the same with the keys pipes never had turned off.
func TestModes(t *testing.T) {
	if defaultMode.lflag&lICANON == 0 || defaultMode.lflag&lECHO == 0 || defaultMode.lflag&lISIG == 0 {
		t.Errorf("defaultMode.lflag = %#x, want ICANON|ECHO|ISIG", defaultMode.lflag)
	}
	if defaultMode.oflag != oOPOST|oONLCR || defaultMode.iflag&iICRNL == 0 {
		t.Errorf("defaultMode iflag %#x oflag %#x, want ICRNL and OPOST|ONLCR", defaultMode.iflag, defaultMode.oflag)
	}
	for key, want := range map[int]byte{vINTR: 0x03, vQUIT: 0x1c, vERASE: 0x7f, vKILL: 0x15, vEOF: 0x04, vSUSP: 0x1a, vWERASE: 0x17, vMIN: 1} {
		if got := defaultMode.cc[key]; got != want {
			t.Errorf("defaultMode.cc[%d] = %#x, want %#x", key, got, want)
		}
	}
	for _, key := range []int{vQUIT, vSUSP, vWERASE} {
		if pipesMode.cc[key] != 0 {
			t.Errorf("pipesMode.cc[%d] = %#x, want 0: pipes never had that key", key, pipesMode.cc[key])
		}
	}
	if k, ok := defaultMode.signalFor(0x1a); !ok || k != sigSuspend {
		t.Errorf("signalFor(^Z) = %v %v, want sigSuspend", k, ok)
	}
	if _, ok := pipesMode.signalFor(0x1a); ok {
		t.Error("pipesMode turns ^Z into a signal; pipes drop it")
	}
	if _, ok := defaultMode.signalFor(0); ok {
		t.Error("NUL matched a disabled key")
	}
}
