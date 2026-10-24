// Package types is the shapes in tunneld's answers that need nothing from
// v1alpha1, named once so every answer that carries one says it the same way.
package types

// Machine is the run's own process, as its .env holds it (HOSTNAME,
// USERNAME, PID, PWD and the tunnel's LIBTUNNEL_HOSTNAME); a line the file
// does not hold is left out.
type Machine struct {
	Tunnel   string `json:"tunnel,omitempty" jsonschema:"the public hostname of this machine's tunnel: its terminals are at https://<tunnel>/, or https://<tunnel>/?n for the origin at index n"`
	Hostname string `json:"hostname,omitempty" jsonschema:"this machine's hostname"`
	Username string `json:"username,omitempty" jsonschema:"the user tunneld runs as on this machine"`
	PID      int    `json:"pid,omitempty" jsonschema:"tunneld's process id on this machine"`
	PWD      string `json:"pwd,omitempty" jsonschema:"the directory tunneld was started in on this machine"`
}
