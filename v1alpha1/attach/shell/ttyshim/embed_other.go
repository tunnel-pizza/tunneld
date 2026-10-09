//go:build !linux || !(amd64 || arm64)

package ttyshim

var object, terminfo []byte
