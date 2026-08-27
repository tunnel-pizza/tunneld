// Package sites holds the pages the examples serve: plain HTML, a handful of
// CSS rules, no build step.
//
// It is the one thing examples/ shares. Each example's main.go stays a
// standalone, copy-pasteable starter — the sample pages are not what anyone
// copies, and keeping one of each means a fix to the scrolling or the sticky
// header lands everywhere at once instead of drifting between examples.
package sites

import "embed"

// FS carries the pages. Read one with FS.ReadFile("frontend.html").
//
//go:embed *.html
var FS embed.FS
