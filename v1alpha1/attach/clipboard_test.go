package attach

import (
	"strings"
	"testing"
)

// TestOSC52 pins how a copy is sent to a viewer's terminal: plain OSC 52, or
// wrapped for the multiplexer the console runs inside, and not at all when it
// is longer than a terminal will take — with the chip saying which.
func TestOSC52(t *testing.T) {
	const hi = "aGk=" // "hi"
	env := func(kv map[string]string) func(string) string {
		return func(k string) string { return kv[k] }
	}
	for name, tc := range map[string]struct {
		text      string
		getenv    func(string) string
		seq, chip string
	}{
		"a tab: plain":     {"hi", nil, "\x1b]52;c;" + hi + "\a", "copied"},
		"a bare console":   {"hi", env(map[string]string{"TERM": "xterm-256color"}), "\x1b]52;c;" + hi + "\a", "copied"},
		"inside tmux":      {"hi", env(map[string]string{"TMUX": "/tmp/tmux-501/default,1,0", "TERM": "tmux-256color"}), "\x1bPtmux;\x1b\x1b]52;c;" + hi + "\a\x1b\\", "copied (tmux: needs allow-passthrough)"},
		"tmux says screen": {"hi", env(map[string]string{"TMUX": "/tmp/tmux-501/default,1,0", "TERM": "screen-256color"}), "\x1bPtmux;\x1b\x1b]52;c;" + hi + "\a\x1b\\", "copied (tmux: needs allow-passthrough)"},
		"inside screen":    {"hi", env(map[string]string{"TERM": "screen.xterm-256color"}), "\x1bP\x1b]52;c;" + hi + "\a\x1b\\", "copied"},
		"at the limit":     {strings.Repeat("a", 56244), nil, "", "copied"},            // 74992 encoded
		"past the limit":   {strings.Repeat("a", 56247), nil, "", "too large to copy"}, // 74996 encoded
		"nothing to copy":  {"", nil, "", ""},
	} {
		t.Run(name, func(t *testing.T) {
			seq, chip := osc52(tc.text, tc.getenv)
			if chip != tc.chip {
				t.Errorf("chip = %q, want %q", chip, tc.chip)
			}
			if tc.seq != "" && seq != tc.seq {
				t.Errorf("seq = %q, want %q", seq, tc.seq)
			}
			if tc.chip == "too large to copy" && seq != "" {
				t.Errorf("seq = %d bytes, want nothing sent", len(seq))
			}
			if name == "at the limit" && !strings.HasPrefix(seq, "\x1b]52;c;") {
				t.Errorf("seq at the limit = %.20q…, want it sent", seq)
			}
		})
	}
}
