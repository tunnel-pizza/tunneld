// Package types is what the MCP tools answer: the output each one returns,
// named once so the tool and the tool error that answers for it agree.
package types

import (
	v1types "github.com/tunnel-pizza/tunneld/v1/types"
	"github.com/tunnel-pizza/tunneld/v1alpha1/attach"
)

// Reply is what terminal_wait and terminal_exec answer: the output read, and
// how the wait ended.
type Reply struct {
	attach.Output
	Matched   bool `json:"matched" jsonschema:"whether until matched"`
	TimedOut  bool `json:"timed_out" jsonschema:"whether timeout_ms passed first; neither this nor matched means the program fell quiet"`
	Truncated bool `json:"truncated" jsonschema:"whether text was cut to its last 64 KiB"`
}

// Screen is what terminal_screen answers: the terminal's screen as it is.
type Screen = attach.Screen

// Read is what terminal_read answers.
type Read struct {
	attach.Output
	Truncated bool `json:"truncated" jsonschema:"whether text was cut to its last 64 KiB"`
}

// Sessions is what terminal_sessions answers.
type Sessions struct {
	Machine  v1types.Machine   `json:"machine"`
	Sessions []v1types.Session `json:"sessions"`
}
