//go:build linux && amd64

package ttyshim

import _ "embed"

//go:embed ttyshim-amd64.so
var object []byte

//go:embed terminfo/x/xterm-256color
var terminfo []byte
