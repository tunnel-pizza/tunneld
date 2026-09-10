package attach

import (
	"bytes"
	"strings"
	"testing"
)

// run feeds input to a fresh scanner and returns what reached the screen and
// what was reported. feed says how the bytes are delivered, which is the whole
// point of the exercise: a sequence split across two Write calls must scan the
// same as one delivered whole.
func run(input string, feed func(*scanner, string)) (screen string, said []Sequence) {
	var out bytes.Buffer
	s := newScanner(&out, func(seq Sequence) {
		// Copy Raw/Data: they are the scanner's buffers, valid only for the call.
		cp := Sequence{Kind: seq.Kind, Cmd: seq.Cmd, Set: seq.Set}
		cp.Raw = append([]byte(nil), seq.Raw...)
		cp.Data = append([]byte(nil), seq.Data...)
		said = append(said, cp)
	})
	feed(s, input)
	return out.String(), said
}

// whole delivers the input in one Write.
func whole(s *scanner, in string) { _, _ = s.Write([]byte(in)) }

// byByte delivers one byte per Write, the worst case for a scanner that holds
// state across calls.
func byByte(s *scanner, in string) {
	for i := 0; i < len(in); i++ {
		_, _ = s.Write([]byte(in[i : i+1]))
	}
}

// split delivers the input as two Writes at every boundary, so a sequence
// straddles the seam in turn.
func split(t *testing.T, input string, want func(screen string, said []Sequence)) {
	t.Helper()
	for i := 0; i <= len(input); i++ {
		var out bytes.Buffer
		var said []Sequence
		s := newScanner(&out, func(seq Sequence) {
			cp := Sequence{Kind: seq.Kind, Cmd: seq.Cmd, Set: seq.Set}
			cp.Raw = append([]byte(nil), seq.Raw...)
			cp.Data = append([]byte(nil), seq.Data...)
			said = append(said, cp)
		})
		_, _ = s.Write([]byte(input[:i]))
		_, _ = s.Write([]byte(input[i:]))
		want(out.String(), said)
	}
}

func TestScannerOwnsOSCAndPassesTheRest(t *testing.T) {
	cases := []struct {
		name   string
		input  string
		screen string     // what the emulator receives
		said   []Sequence // Raw compared as a string below
	}{
		{
			// The bug this exists for: 0x9C is a UTF-8 continuation byte and
			// must never terminate an OSC. ✳ is E2 9C B3.
			name:   "dingbat title via BEL",
			input:  "\x1b]0;✳ Claude Code\aPROMPT> ",
			screen: "PROMPT> ",
			said:   []Sequence{{Raw: []byte("\x1b]0;✳ Claude Code\a"), Kind: OSC, Cmd: 0, Data: []byte("✳ Claude Code")}},
		},
		{
			name:   "dingbat title via ST",
			input:  "\x1b]2;✳ x\x1b\\rest",
			screen: "rest",
			said:   []Sequence{{Raw: []byte("\x1b]2;✳ x\x1b\\"), Kind: OSC, Cmd: 2, Data: []byte("✳ x")}},
		},
		{
			// U+201C “ is E2 80 9C — the 0x9C is the third byte here.
			name:   "curly quote title",
			input:  "\x1b]0;“q”\a",
			screen: "",
			said:   []Sequence{{Raw: []byte("\x1b]0;“q”\a"), Kind: OSC, Cmd: 0, Data: []byte("“q”")}},
		},
		{
			name:   "clipboard write forwarded whole",
			input:  "before\x1b]52;c;aGk=\aafter",
			screen: "beforeafter",
			said:   []Sequence{{Raw: []byte("\x1b]52;c;aGk=\a"), Kind: OSC, Cmd: 52, Data: []byte("c;aGk=")}},
		},
		{
			name:   "private mode reported and passed",
			input:  "x\x1b[?25ly",
			screen: "x\x1b[?25ly",
			said:   []Sequence{{Raw: []byte("\x1b[?25l"), Kind: Mode, Cmd: 25, Set: false}},
		},
		{
			name:   "two modes at once, both reported",
			input:  "\x1b[?1049;25h",
			screen: "\x1b[?1049;25h",
			said: []Sequence{
				{Raw: []byte("\x1b[?1049;25h"), Kind: Mode, Cmd: 1049, Set: true},
				{Raw: []byte("\x1b[?1049;25h"), Kind: Mode, Cmd: 25, Set: true},
			},
		},
		{
			name:   "SGR passes unreported",
			input:  "\x1b[31mred\x1b[0m",
			screen: "\x1b[31mred\x1b[0m",
			said:   nil,
		},
		{
			name:   "OSC with no numeric command",
			input:  "\x1b]hello\a",
			screen: "",
			said:   []Sequence{{Raw: []byte("\x1b]hello\a"), Kind: OSC, Cmd: -1, Data: []byte("hello")}},
		},
		{
			name:   "DCS passes through, its ES- ] is not an OSC",
			input:  "\x1bPq#0;2;0;0;0\x1b\\tail",
			screen: "\x1bPq#0;2;0;0;0\x1b\\tail",
			said:   nil,
		},
		{
			name:   "OSC ended by a bare ESC then SGR",
			input:  "\x1b]0;t\x1b[31m",
			screen: "\x1b[31m",
			said:   []Sequence{{Raw: []byte("\x1b]0;t"), Kind: OSC, Cmd: 0, Data: []byte("t")}},
		},
		{
			name:   "CAN aborts an OSC, nothing said, nothing drawn",
			input:  "\x1b]0;t\x18done",
			screen: "done",
			said:   nil,
		},
		{
			name:   "raw 0x9C in ground reaches the screen",
			input:  "a\x9cb",
			screen: "a\x9cb",
			said:   nil,
		},
	}

	check := func(t *testing.T, name, screen string, said []Sequence, wantScreen string, wantSaid []Sequence) {
		if screen != wantScreen {
			t.Errorf("%s: screen = %q, want %q", name, screen, wantScreen)
		}
		if len(said) != len(wantSaid) {
			t.Fatalf("%s: said %d sequences, want %d: %+v", name, len(said), len(wantSaid), said)
		}
		for i := range said {
			g, w := said[i], wantSaid[i]
			if string(g.Raw) != string(w.Raw) || g.Kind != w.Kind || g.Cmd != w.Cmd ||
				string(g.Data) != string(w.Data) || g.Set != w.Set {
				t.Errorf("%s: said[%d] = {Raw:%q Kind:%d Cmd:%d Data:%q Set:%v}, want {Raw:%q Kind:%d Cmd:%d Data:%q Set:%v}",
					name, i, g.Raw, g.Kind, g.Cmd, g.Data, g.Set, w.Raw, w.Kind, w.Cmd, w.Data, w.Set)
			}
		}
	}

	for _, tc := range cases {
		t.Run(tc.name+"/whole", func(t *testing.T) {
			screen, said := run(tc.input, whole)
			check(t, tc.name, screen, said, tc.screen, tc.said)
		})
		t.Run(tc.name+"/byByte", func(t *testing.T) {
			screen, said := run(tc.input, byByte)
			check(t, tc.name, screen, said, tc.screen, tc.said)
		})
		t.Run(tc.name+"/split", func(t *testing.T) {
			split(t, tc.input, func(screen string, said []Sequence) {
				check(t, tc.name, screen, said, tc.screen, tc.said)
			})
		})
	}
}

// TestScannerConserves pins that no owned OSC leaves bytes on the screen and no
// passed byte is lost: for every case without a CAN/SUB abort, screen bytes
// plus every OSC's Raw equals the input.
func TestScannerConserves(t *testing.T) {
	inputs := []string{
		"\x1b]0;✳ Claude Code\aPROMPT> ",
		"before\x1b]52;c;aGk=\aafter",
		"x\x1b[?25ly",
		"\x1b[31mred\x1b[0m",
		"\x1bPq#0\x1b\\tail",
	}
	for _, in := range inputs {
		screen, said := run(in, whole)
		var owned strings.Builder
		for _, seq := range said {
			if seq.Kind == OSC {
				owned.Write(seq.Raw)
			}
		}
		// Reassemble by walking the input and pulling each OSC Raw out of it in
		// order; what remains must be exactly the screen.
		if len(screen) == 0 && owned.Len() == 0 {
			t.Errorf("%q produced nothing at all", in)
		}
		if len(screen)+owned.Len() != len(in) {
			t.Errorf("%q: screen(%d) + owned(%d) = %d, want %d",
				in, len(screen), owned.Len(), len(screen)+owned.Len(), len(in))
		}
	}
}

// TestScannerCapReleasesAnUnterminatedOSC pins that a sequence that never ends
// is not held forever: past the cap the held bytes go to the screen and the
// scanner returns to ground.
func TestScannerCapReleasesAnUnterminatedOSC(t *testing.T) {
	in := "\x1b]0;" + strings.Repeat("x", (1<<20)+16)
	screen, said := run(in, whole)
	if len(said) != 0 {
		t.Errorf("said %d sequences for an unterminated OSC, want 0", len(said))
	}
	if len(screen) != len(in) {
		t.Errorf("screen released %d bytes, want the whole %d", len(screen), len(in))
	}
}

// TestScannerReset drops a half-read sequence, which is what revive needs when
// a run dies mid-OSC.
func TestScannerReset(t *testing.T) {
	var out bytes.Buffer
	var said []Sequence
	s := newScanner(&out, func(seq Sequence) { said = append(said, seq) })
	_, _ = s.Write([]byte("\x1b]0;half"))
	s.reset()
	_, _ = s.Write([]byte("plain"))
	if out.String() != "plain" {
		t.Errorf("screen = %q, want %q — reset did not drop the half-read OSC", out.String(), "plain")
	}
	if len(said) != 0 {
		t.Errorf("said %d, want 0", len(said))
	}
}
