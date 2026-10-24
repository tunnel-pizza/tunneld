package types

import "github.com/tunnel-pizza/tunneld/v1/origin"

// Session is one origin, with the address the other terminal tools take it
// at.
type Session struct {
	origin.Origin
	Address string `json:"address,omitempty" jsonschema:"the origin's public address, the url the other terminal tools take: https://<tunnel>/, or https://<tunnel>/?n when the run has more than one origin"`
}
