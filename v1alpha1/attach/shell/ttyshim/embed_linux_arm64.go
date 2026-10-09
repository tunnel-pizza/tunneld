//go:build linux && arm64

package ttyshim

import _ "embed"

//go:embed ttyshim-arm64.so
var object []byte

//go:embed terminfo/x/xterm-256color
var terminfo []byte
