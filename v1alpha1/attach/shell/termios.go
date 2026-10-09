package shell

// settings is a terminal's settings in Linux's layout and values, whatever
// tunneld is built for: the shim runs on Linux, and pipes borrow the same
// shape to say what they do.
type settings struct {
	iflag, oflag, cflag, lflag uint32
	line                       uint8
	cc                         [32]uint8
	ispeed, ospeed             uint32
}

// Linux's termios flag values.
const (
	iICRNL = 0x100
	iIXON  = 0x400

	oOPOST = 0x1
	oONLCR = 0x4

	cB38400 = 0xf
	cCS8    = 0x30
	cCREAD  = 0x80
	cHUPCL  = 0x400

	lISIG    = 0x1
	lICANON  = 0x2
	lECHO    = 0x8
	lECHOE   = 0x10
	lECHOK   = 0x20
	lECHOCTL = 0x200
	lECHOKE  = 0x800
	lIEXTEN  = 0x8000
)

// Linux's c_cc indices.
const (
	vINTR    = 0
	vQUIT    = 1
	vERASE   = 2
	vKILL    = 3
	vEOF     = 4
	vTIME    = 5
	vMIN     = 6
	vSTART   = 8
	vSTOP    = 9
	vSUSP    = 10
	vREPRINT = 12
	vDISCARD = 13
	vWERASE  = 14
	vLNEXT   = 15
)

// defaultMode is a fresh Linux pseudo-terminal's settings.
var defaultMode = func() settings {
	s := settings{
		iflag:  iICRNL | iIXON,
		oflag:  oOPOST | oONLCR,
		cflag:  cB38400 | cCS8 | cCREAD | cHUPCL,
		lflag:  lISIG | lICANON | lECHO | lECHOE | lECHOK | lECHOCTL | lECHOKE | lIEXTEN,
		ispeed: cB38400,
		ospeed: cB38400,
	}
	s.cc[vINTR], s.cc[vQUIT], s.cc[vERASE], s.cc[vKILL] = 0x03, 0x1c, 0x7f, 0x15
	s.cc[vEOF], s.cc[vTIME], s.cc[vMIN] = 0x04, 0, 1
	s.cc[vSTART], s.cc[vSTOP], s.cc[vSUSP] = 0x11, 0x13, 0x1a
	s.cc[vREPRINT], s.cc[vDISCARD], s.cc[vWERASE], s.cc[vLNEXT] = 0x12, 0x0f, 0x17, 0x16
	return s
}()

// pipesMode is what a program on bare pipes gets: defaultMode without the
// keys pipes never had, so ^\, ^Z and ^W stay dropped.
var pipesMode = func() settings {
	s := defaultMode
	s.cc[vQUIT], s.cc[vSUSP], s.cc[vWERASE] = 0, 0, 0
	return s
}()

// signalKey is a key a terminal turns into a signal.
type signalKey int

const (
	sigInterrupt signalKey = iota
	sigQuit
	sigSuspend
)

// signalFor is the signal b stands for under s, if any. A key set to 0 is
// disabled and matches nothing.
func (s settings) signalFor(b byte) (signalKey, bool) {
	if b == 0 {
		return 0, false
	}
	switch b {
	case s.cc[vINTR]:
		return sigInterrupt, true
	case s.cc[vQUIT]:
		return sigQuit, true
	case s.cc[vSUSP]:
		return sigSuspend, true
	}
	return 0, false
}
