package attach

import (
	"encoding/base64"
	"strings"
)

// osc52Max is the longest encoded payload a copy is sent with. Terminals cap
// what an OSC 52 write may carry and drop anything longer without a word;
// k9s settled on this bound (K9S_OSC52_MAX's default) for the same reason, and
// a copy past it is refused here rather than sent to be lost.
const osc52Max = 74994

// screenChunk is the most of a write one of screen's DCS strings carries,
// as osc52.sh and vim-oscyank send it: screen truncates a longer one.
const screenChunk = 76

// osc52 is how text reaches a viewer's clipboard: the OSC 52 write to send,
// and the chip that says what became of it. A terminal never acknowledges the
// write, so the chip says only what is known before sending it.
//
// getenv is the environment of the terminal the frame is drawn on: the
// console's own process, which is the viewer's terminal there. Inside tmux it
// goes twice, plain and in tmux's passthrough: tmux forwards the first with
// set-clipboard on and the second with allow-passthrough on, the defaults
// being neither, and the same text landing twice is harmless. The chip says
// "sent to tmux", since whether either got through is tmux's to know. Inside
// screen it goes in screen's DCS, in pieces of 76 bytes: screen keeps only a
// few hundred bytes of one. Nil, for a tab, is the browser's terminal, which
// takes OSC 52 plain.
//
// Nothing to copy is no write and no chip; too much is no write and a chip
// that says why.
func osc52(text string, getenv func(string) string) (seq, chip string) {
	if text == "" {
		return "", ""
	}
	b64 := base64.StdEncoding.EncodeToString([]byte(text))
	if len(b64) > osc52Max {
		return "", "too large to copy"
	}
	plain := "\x1b]52;c;" + b64 + "\a"
	if getenv == nil {
		return plain, "copied"
	}
	switch {
	case getenv("TMUX") != "":
		// Every ESC inside tmux's passthrough is doubled.
		return plain + "\x1bPtmux;\x1b" + plain + "\x1b\\", "sent to tmux"
	case strings.HasPrefix(getenv("TERM"), "screen"):
		var b strings.Builder
		for i := 0; i < len(plain); i += screenChunk {
			b.WriteString("\x1bP" + plain[i:min(i+screenChunk, len(plain))] + "\x1b\\")
		}
		return b.String(), "copied"
	}
	return plain, "copied"
}
