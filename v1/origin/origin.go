// Package origin is what one origin of a run is: the value every package
// passes around, and what it reads off one.
package origin

import (
	"net"
	"net/url"
	"path"
)

const (
	// AttachScheme names a terminal attached to something already running as
	// an origin instead of an HTTP service: an
	// attach://<provider>/<reference> origin serves that terminal on the same
	// public hostname and the same ?n index as any other origin.
	//
	// A served origin is spelled by the verb, and the authority beside it says
	// where the verb happens. attach://dockerd/api attaches to a container's
	// PID 1; the day there is a second way into the same container it is
	// exec://dockerd/api, differing in the word that says what is being done
	// rather than in the word that says to what. DockerProvider is the only
	// authority this scheme answers today.
	AttachScheme Scheme = "attach"

	// ExecScheme names something run as an origin instead of an HTTP service:
	// an exec:///path/to/program origin runs that program and exposes its
	// terminal, on the same public hostname and the same ?n index as any
	// other origin.
	//
	// The empty authority is this machine, which is why the path is absolute
	// and why exec:// is the only served scheme with no provider to name. An
	// authority is a provider — exec://<provider>/<reference> is the same verb
	// somewhere else — with one exception below.
	//
	// ExecScheme is also what a bare argument becomes when this machine can
	// run it. A word that names a command on $PATH is rewritten while origins
	// are settled, so `tunneld htop` says what somebody meant rather than
	// becoming http://htop — a hostname that resolves nowhere, minted and
	// published before anyone finds out. The check is the host's own PATH
	// lookup, so the same argument is a program on one machine and a hostname
	// on another; that is the cost of the shorthand, and the reason an
	// explicit scheme always wins.
	//
	// The exception: exec://<word>, an authority with nothing after it, is
	// looked up the same way before it is read as a provider. Nothing can be
	// asked of a provider without a reference anyway, so the reading that can
	// succeed is preferred to the one that cannot. Either way the rewrite
	// carries the resolved path rather than the word — exec:///usr/bin/htop,
	// not exec://htop — because a bare name says which program only on the
	// machine that looked it up.
	ExecScheme Scheme = "exec"

	// HttpScheme names an address the tunnel dials: an http:// origin, and
	// an https:// one too where a scheme says what kind of origin something
	// is rather than how it is dialed.
	HttpScheme Scheme = "http"

	// ArgKey is the query key a program's arguments ride under in an exec://
	// origin: exec:///usr/bin/claude?arg=--resume&arg=--model runs claude
	// with two arguments, in that order. It is how the words after a program
	// on the command line reach it, and how an origin that arrives through
	// OriginsEnv or WithOrigin says the same thing.
	ArgKey = "arg"

	// DockerProvider is the authority that names the Docker daemon in a served
	// origin: the dockerd in attach://dockerd/<container-name-or-id>.
	//
	// The daemon, not the container — the same reading as dockerd's own socket
	// — because the container reference is what follows it. It is a separate
	// word from the scheme because the two are separate axes now: the scheme
	// says what is being done, and this says by whom.
	DockerProvider Provider = "dockerd"
)

// Scheme is what an origin is: ExecScheme, a program; AttachScheme, a
// container; HttpScheme, an address. Its own type, so an origin's Scheme is
// never mistaken for its URI's, which can say https where this says http.
type Scheme string

// Provider is who a served origin is asked of: DockerProvider, or "" for
// this machine, the empty authority of exec:///bin/sh.
type Provider string

// Origin is one origin of a run, worked out once from the URL it was settled
// as and passed by value after that. Its public address is not here: it is
// not known when origins are settled, and it can change while the run lasts.
//
// Encoded, as an agent is told about it, it is index, scheme, provider and
// ref: never its URI or its Args, since a program's arguments can hold a
// secret and the URI carries them.
type Origin struct {
	// Index is its place in the run's origins: the n of ?n.
	Index int `json:"index" jsonschema:"the origin's index among all the run's origins, 0 the first: in a URL a bare ?1 (not ?n=1) picks origin 1, and a single origin needs none"`
	// URI is the origin as settled: exec:///bin/zsh?arg=-l,
	// attach://dockerd/web, http://localhost:3000.
	URI url.URL `json:"-"`
	// Scheme is what it is: ExecScheme, AttachScheme, or HttpScheme for an
	// http or https address.
	Scheme Scheme `json:"scheme" jsonschema:"what the origin is: exec, a program on this machine, or attach, a container, both terminals the terminal tools type into and read; or http, a web address the tunnel forwards to, which they cannot"`
	// Provider is the authority a served origin names, "" for this machine
	// and for an address.
	Provider Provider `json:"provider,omitempty" jsonschema:"who it is asked of: dockerd for a container, none for this machine"`
	// Ref is what is asked for: a program's path (/bin/zsh), a container
	// (web), or an address's host and port with any path (localhost:3000).
	Ref string `json:"ref" jsonschema:"an exec origin's program path (never its arguments), an attach origin's container, an http origin's host and port"`
	// Args is an exec origin's arguments, its ArgKey values, in order.
	Args []string `json:"-"`
}

// URL is the origin as settled, as a copy with credentials of its own:
// changing what comes back changes nothing in the run.
func (o Origin) URL() *url.URL {
	u := o.URI
	if u.User != nil {
		user := *u.User
		u.User = &user
	}
	return &u
}

// Label is the origin as a person reads it: a program without its arguments.
//
// The arguments ride the origin as a query because that is how they travel —
// argv, the environment and a seed all spell them the same way — but a query
// is a carrier, not a label. What the map says a tunnel reaches is the
// program; how it was started is in the cache file beside the spec, and in
// the key that names the file. An http origin's query is part of its address
// and stays.
func (o Origin) Label() string {
	u := o.URL()
	if o.Scheme == ExecScheme {
		u.RawQuery = ""
	}
	return u.String()
}

// Bare is the origin as scheme, host and path, escaped as a URL. A query, a
// fragment or credentials can hold a secret, so none of them is in it: what a
// provider is told an origin is.
func (o Origin) Bare() string {
	u := o.URL()
	bare := url.URL{Scheme: u.Scheme, Host: u.Host, Path: u.Path, Opaque: u.Opaque}
	return bare.String()
}

// Name is the origin's short name: a program or container by its base name,
// an address on this machine by its port (":3000"), any other by its host.
func (o Origin) Name() string {
	switch o.Scheme {
	case ExecScheme, AttachScheme:
		return path.Base(o.Ref)
	}
	u := o.URL()
	host, port := u.Hostname(), u.Port()
	if ip := net.ParseIP(host); port != "" && (host == "localhost" || ip != nil && ip.IsLoopback()) {
		return ":" + port
	}
	return u.Host
}
