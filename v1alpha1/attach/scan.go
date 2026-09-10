package attach

import (
	"bytes"
	"io"
	"strconv"
)

// Kind is which of the two shapes a Sequence is.
type Kind int

const (
	// OSC is owned: the scanner does not write it to the screen, and the
	// emulator receives it only if a sink hands it back.
	OSC Kind = iota
	// Mode is observed: a private-mode CSI (CSI ? Pm h|l) that has already
	// been written to the screen and is reported so the frame can act on it.
	Mode
)

// Sequence is one thing a program said about itself, exactly as it said it.
//
// Raw and Data are the scanner's own buffers and are valid only for the
// duration of the report call. A sink that keeps either copies it.
type Sequence struct {
	Raw  []byte // introducer through terminator
	Kind Kind
	Cmd  int    // OSC: the number before the first ';', or -1. Mode: the mode number
	Data []byte // OSC: everything after the first ';', without the terminator
	Set  bool   // Mode: h (set) rather than l (reset)
}

// maxOSC bounds an unterminated OSC. Past it the held bytes are not a
// sequence — they are released to the screen as they are and the scanner
// starts over. Large enough for any clipboard write a terminal would accept.
const maxOSC = 1 << 20

type scanState int

const (
	scanGround scanState = iota
	scanEsc              // saw ESC
	scanOSC              // inside an OSC string
	scanOSCEsc           // inside an OSC, saw ESC (ST if a '\' follows)
	scanCSI              // inside a CSI
	scanString           // inside DCS/SOS/PM/APC — passed through, unreported
)

// scanner reads a terminal byte stream. It hands every OSC and every
// private-mode CSI to report, and writes everything else to screen unchanged.
//
// It is 7-bit by construction: the only bytes it ever branches on are the C0
// controls BEL/CAN/SUB, ESC, and ASCII after an ESC. Every byte at or above
// 0x80 is data in every state, which is why a UTF-8 character whose encoding
// contains 0x9C — ✳, “, 末 — is never cut. That is exactly xterm's rule in
// UTF-8 mode: a C1 control is recognised only as U+0080–U+009F, never as a raw
// byte. An app that sends 8-bit introducers is unsupported; they are not valid
// in a UTF-8 stream.
type scanner struct {
	screen io.Writer
	report func(Sequence)
	state  scanState
	held   []byte // the in-progress sequence, introducer included
}

func newScanner(screen io.Writer, report func(Sequence)) *scanner {
	return &scanner{screen: screen, report: report, state: scanGround}
}

// reset drops any half-read sequence and returns to ground. revive calls it: a
// run can die mid-sequence, and the next run must not inherit half of one.
func (s *scanner) reset() {
	s.held = s.held[:0]
	s.state = scanGround
}

// Write feeds bytes through the scanner. Ground bytes are written to the screen
// in runs, so a chunk that is all text costs one write; only an open sequence
// is held across the call.
func (s *scanner) Write(p []byte) (int, error) {
	gs := -1 // start of a ground run in p, or -1
	flush := func(end int) {
		if gs >= 0 && end > gs {
			_, _ = s.screen.Write(p[gs:end])
		}
		gs = -1
	}
	// emit writes the held bytes (which may span chunks) to the screen.
	emit := func() {
		_, _ = s.screen.Write(s.held)
		s.held = s.held[:0]
	}
	for i := 0; i < len(p); i++ {
		b := p[i]
		switch s.state {
		case scanGround:
			if b == 0x1b {
				flush(i)
				s.held = append(s.held[:0], b)
				s.state = scanEsc
			} else if gs < 0 {
				gs = i
			}

		case scanEsc:
			switch {
			case b == ']':
				s.held = append(s.held, b)
				s.state = scanOSC
			case b == '[':
				s.held = append(s.held, b)
				s.state = scanCSI
			case b == 'P' || b == 'X' || b == '^' || b == '_':
				s.held = append(s.held, b)
				emit()
				s.state = scanString
			default:
				s.held = append(s.held, b)
				emit()
				s.state = scanGround
			}

		case scanOSC:
			switch b {
			case 0x07: // BEL
				s.held = append(s.held, b)
				s.reportOSC()
				s.held = s.held[:0]
				s.state = scanGround
			case 0x1b: // ESC — maybe ST
				s.held = append(s.held, b)
				s.state = scanOSCEsc
			case 0x18, 0x1a: // CAN, SUB — abort, nothing said, nothing drawn
				s.held = s.held[:0]
				s.state = scanGround
			default:
				s.held = append(s.held, b)
				if len(s.held) > maxOSC {
					emit()
					s.state = scanGround
				}
			}

		case scanOSCEsc:
			if b == '\\' { // ST
				s.held = append(s.held, b)
				s.reportOSC()
				s.held = s.held[:0]
				s.state = scanGround
			} else {
				// The ESC ended the OSC; it did not extend it. Report the OSC
				// without that trailing ESC, then let the ESC introduce
				// whatever this byte begins.
				s.held = s.held[:len(s.held)-1]
				s.reportOSC()
				s.held = append(s.held[:0], 0x1b)
				s.state = scanEsc
				i-- // reprocess b in scanEsc
			}

		case scanCSI:
			switch {
			case (b >= 0x30 && b <= 0x3f) || (b >= 0x20 && b <= 0x2f):
				// parameter or intermediate byte
				s.held = append(s.held, b)
			case b >= 0x40 && b <= 0x7e:
				// final byte. Written to the screen before it is reported, so
				// a Mode's sink sees the CSI already in effect — reportModes
				// reads s.held, so the clear waits until after it returns
				// rather than going through emit, which would clear early.
				s.held = append(s.held, b)
				_, _ = s.screen.Write(s.held)
				s.reportModes()
				s.held = s.held[:0]
				s.state = scanGround
			case b == 0x1b:
				emit()
				s.held = append(s.held[:0], 0x1b)
				s.state = scanEsc
			default:
				s.held = append(s.held, b)
				emit()
				s.state = scanGround
			}

		case scanString:
			if b == 0x1b {
				flush(i)
				s.held = append(s.held[:0], b)
				s.state = scanEsc
			} else if gs < 0 {
				gs = i
			}
		}
	}
	flush(len(p))
	return len(p), nil
}

// reportOSC parses held (ESC ] <payload> <terminator>) and reports it.
func (s *scanner) reportOSC() {
	payload := s.held[2:] // drop ESC ]
	switch {
	case len(payload) >= 2 && payload[len(payload)-2] == 0x1b && payload[len(payload)-1] == '\\':
		payload = payload[:len(payload)-2]
	case len(payload) >= 1 && payload[len(payload)-1] == 0x07:
		payload = payload[:len(payload)-1]
	}
	cmd, data := splitOSC(payload)
	s.report(Sequence{Raw: s.held, Kind: OSC, Cmd: cmd, Data: data})
}

// splitOSC reads the leading numeric command and the payload after the first
// ';'. No leading digit is command -1 and the whole payload as data.
func splitOSC(payload []byte) (cmd int, data []byte) {
	i := 0
	for i < len(payload) && payload[i] >= '0' && payload[i] <= '9' {
		i++
	}
	if i == 0 {
		return -1, payload
	}
	n, _ := strconv.Atoi(string(payload[:i]))
	if i < len(payload) && payload[i] == ';' {
		return n, payload[i+1:]
	}
	return n, nil
}

// reportModes reports each mode in a private-mode CSI (ESC [ ? Pm ; Pm h|l).
// A CSI that is not '?'-prefixed reports nothing.
func (s *scanner) reportModes() {
	if len(s.held) < 4 || s.held[2] != '?' {
		return
	}
	set := s.held[len(s.held)-1] == 'h'
	body := s.held[3 : len(s.held)-1]
	for _, part := range bytes.Split(body, []byte{';'}) {
		n, err := strconv.Atoi(string(part))
		if err != nil {
			continue
		}
		s.report(Sequence{Raw: s.held, Kind: Mode, Cmd: n, Set: set})
	}
}
