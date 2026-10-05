package attach

import (
	"encoding/base64"
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
		"a tab: plain":   {"hi", nil, "\x1b]52;c;" + hi + "\a", "copied"},
		"a bare console": {"hi", env(map[string]string{"TERM": "xterm-256color"}), "\x1b]52;c;" + hi + "\a", "copied"},
		// Plain for tmux's set-clipboard on, then its passthrough for
		// allow-passthrough on: whichever it forwards, the same text lands.
		"inside tmux":      {"hi", env(map[string]string{"TMUX": "/tmp/tmux-501/default,1,0", "TERM": "tmux-256color"}), "\x1b]52;c;" + hi + "\a\x1bPtmux;\x1b\x1b]52;c;" + hi + "\a\x1b\\", "sent to tmux"},
		"tmux says screen": {"hi", env(map[string]string{"TMUX": "/tmp/tmux-501/default,1,0", "TERM": "screen-256color"}), "\x1b]52;c;" + hi + "\a\x1bPtmux;\x1b\x1b]52;c;" + hi + "\a\x1b\\", "sent to tmux"},
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

// TestOSC52InsideScreenIsChunked pins screen's limit: it keeps a DCS string
// only up to a few hundred bytes, so the write goes in pieces of 76, each its
// own DCS, the way osc52.sh sends it. Unwrapped, they are the one write.
func TestOSC52InsideScreenIsChunked(t *testing.T) {
	text := strings.Repeat("0123456789", 30)
	seq, chip := osc52(text, func(k string) string { return map[string]string{"TERM": "screen"}[k] })
	if chip != "copied" {
		t.Errorf("chip = %q", chip)
	}
	if !strings.HasPrefix(seq, "\x1bP") || !strings.HasSuffix(seq, "\x1b\\") {
		t.Fatalf("seq = %.30q…, want it in screen's DCS", seq)
	}
	pieces := strings.Split(strings.TrimSuffix(strings.TrimPrefix(seq, "\x1bP"), "\x1b\\"), "\x1b\\\x1bP")
	for _, p := range pieces {
		if len(p) > 76 {
			t.Errorf("a piece of %d bytes, want at most 76", len(p))
		}
	}
	if joined := strings.Join(pieces, ""); joined != "\x1b]52;c;"+base64.StdEncoding.EncodeToString([]byte(text))+"\a" {
		t.Errorf("the pieces joined = %.40q…, want the one OSC 52 write", joined)
	}
}
