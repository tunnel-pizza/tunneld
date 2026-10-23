// Package origins is the local origins a run exposes: the list, and the key
// that says which tunnel they are.
//
// A type rather than a slice because of that key. Two runs are the same tunnel
// when they serve the same origins from the same directory in the same agent
// session, if any, and the spec cache
// files a tunnel under exactly that — so the question "are these two runs the
// same tunnel" has one answer, computed here, rather than one per caller.
package origins

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	v1 "github.com/tunnel-pizza/tunneld/v1"
)

// keyWidth is how much of the digest a key carries. Sixteen hex characters is
// 64 bits: far beyond collision for the number of tunnels one machine has, and
// short enough to read out of a banner and compare by eye.
const keyWidth = 16

// sessionEnv is the environment an agent host names its session in, on what
// it starts.
var sessionEnv = []string{
	"CLAUDE_CODE_SESSION_ID",
}

// Option configures an OriginsImpl at construction.
type Option = v1.Option[*OriginsImpl]

// OriginsImpl is the default Origins: a list, and the directory it was settled
// in. Both are fixed once New returns, but for what Add appends.
type OriginsImpl struct {
	// dir is the working directory this run's identity is scoped to, so two
	// projects serving :3000 are two tunnels rather than one.
	dir     string
	origins []v1.Origin
	// ws is the index of the origin that owns WebSockets, or -1 for none.
	ws int
}

// New returns an OriginsImpl configured by opts, with the process's working
// directory already in it.
//
// A Getwd that fails leaves the directory empty rather than failing: the key
// is then the origins alone, which is a weaker identity and not a broken one —
// the same judgement the cache makes about a machine with no cache directory.
func New(opts ...Option) *OriginsImpl {
	o := &OriginsImpl{ws: -1}
	if wd, err := os.Getwd(); err == nil {
		o.dir = wd
	}
	return v1.Apply(o, opts...)
}

// WithURL adds origins, in order, appending across calls — so a list can be
// assembled from more than one source without the caller joining them first.
// A repeat costs nothing: Key deduplicates, and the run exposes what it was
// given. Each is worked out into a v1.Origin here, once.
func WithURL(urls ...*url.URL) Option {
	return func(o *OriginsImpl) {
		for _, u := range urls {
			o.origins = append(o.origins, originOf(len(o.origins), u))
		}
	}
}

// originOf is u as the origin at index: what it counts as, who it is asked
// of and what for.
//
// The reference is the path, and the authority decides how much of it: with
// a provider named the leading separator belongs to the URL and not to the
// reference (attach://dockerd/api is the container api), and without one the
// empty authority is this machine, so the path is a filesystem path and keeps
// every byte (exec:///usr/bin/top). An address's reference is its host and
// port, with its path but for a bare "/".
func originOf(index int, u *url.URL) v1.Origin {
	o := v1.Origin{Index: index, URI: *u, Scheme: v1.HttpScheme}
	switch scheme := v1.Scheme(u.Scheme); scheme {
	case v1.ExecScheme, v1.AttachScheme:
		o.Scheme = scheme
		o.Provider = v1.Provider(u.Host)
		o.Ref = u.Path
		if u.Host != "" {
			o.Ref = strings.TrimPrefix(o.Ref, "/")
		}
		o.Args = u.Query()[v1.ArgKey]
	default:
		o.Ref = u.Host + strings.TrimSuffix(u.Path, "/")
	}
	return o
}

// WithWebSocket names the origin at index i as the one that owns WebSockets:
// a handshake that carries no index of its own is routed there rather than
// guessed at. A later call replaces an earlier one, since two origins cannot
// both own the sockets; an index outside the list names none.
func WithWebSocket(i int) Option {
	return func(o *OriginsImpl) { o.ws = i }
}

// WithDir replaces the working directory the key is scoped to. What a test
// uses to fix a key without moving the process, and what an embedder uses when
// the directory it runs in is not the one it is serving for.
func WithDir(dir string) Option {
	return func(o *OriginsImpl) { o.dir = dir }
}

// Add appends origins, in order, each renumbered to the place it lands, so a
// list assembled from more than one source keeps every Index its ?n. A repeat
// costs nothing: Key deduplicates.
func (o *OriginsImpl) Add(origins ...v1.Origin) {
	for _, origin := range origins {
		origin = clone(origin)
		origin.Index = len(o.origins)
		o.origins = append(o.origins, origin)
	}
}

// Len implements v1.Origins.
func (o *OriginsImpl) Len() int { return len(o.origins) }

// At implements v1.Origins, as a copy. The caller has already bounded i by
// Len, so an index outside it panics the way a slice does.
func (o *OriginsImpl) At(i int) v1.Origin { return clone(o.origins[i]) }

// All implements v1.Origins, as a copy: Key sorts, and a caller that sorted
// or changed what it was handed would change the run's routing as a side
// effect of asking what it is.
func (o *OriginsImpl) All() []v1.Origin {
	out := make([]v1.Origin, len(o.origins))
	for i, origin := range o.origins {
		out[i] = clone(origin)
	}
	return out
}

// From implements v1.Origins.
func (o *OriginsImpl) From(raw string) (v1.Origin, error) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return v1.Origin{}, fmt.Errorf("%w: %q is not an address on the tunnel", v1.ErrInvalidOrigin, raw)
	}
	// The router's rule: a segment Atoi accepts is a bare numeric parameter;
	// a valued one ("1=x") carries '=' and is the application's own.
	index := 0
	for seg := range strings.SplitSeq(u.RawQuery, "&") {
		if n, err := strconv.Atoi(seg); err == nil {
			index = n
			break
		}
	}
	if index < 0 || index >= len(o.origins) {
		return v1.Origin{}, fmt.Errorf("%w: %q names origin %d, and there are %d", v1.ErrInvalidOrigin, raw, index, len(o.origins))
	}
	return clone(o.origins[index]), nil
}

// clone is origin with nothing shared: its Args and its URI's credentials are
// its own.
func clone(origin v1.Origin) v1.Origin {
	origin.Args = slices.Clone(origin.Args)
	origin.URI = *origin.URL()
	return origin
}

// WebSocket is the index of the origin that owns WebSockets — the one an
// operator marked +ws — and false when none is marked. The marker is routing
// configuration rather than part of any address, so the parser takes it off
// the scheme and keeps it here, and every URL the list hands out is the bare
// address it dials. An index outside the list names none.
//
// A method of this type and not of v1.Origins: the router is its only reader,
// and a method on the public interface is one every implementation outside
// this module would have to grow.
func (o *OriginsImpl) WebSocket() (int, bool) {
	if o.ws < 0 || o.ws >= len(o.origins) {
		return -1, false
	}
	return o.ws, true
}

// Key implements v1.Origins: the directory, each of sessionEnv that is set, and
// the origins, sorted and deduplicated, hashed together — each origin whole, a
// program's arguments included. Without a session the key is what it was
// before sessions counted, so a run outside an agent keeps its hostname.
//
// Sorted because the order is the run's business and not the tunnel's — ?0 and
// ?1 index the list, but serving the same two things in the other order is the
// same tunnel. Deduplicated for the same reason. Newline-joined so the parts
// cannot run together: a directory and an origin concatenated raw could be
// split two ways and collide.
//
// A program's arguments used to be set aside here, on the reading that claude
// and claude --resume are one tunnel started two ways. They are not once the
// arguments say which thing is running: claude --resume ABC and claude
// --resume DEF are two sessions, and one key put them on one hostname — two
// connectors on one tunnel when run together, a link that quietly changed
// what it opened when run apart (#182). The arguments ride the origin as a
// query, in order, so the whole URL is the whole invocation.
func (o *OriginsImpl) Key() string {
	parts := make([]string, 0, len(o.origins))
	for _, origin := range o.origins {
		parts = append(parts, origin.URL().String())
	}
	slices.Sort(parts)
	parts = slices.Compact(parts)

	var b strings.Builder
	b.WriteString(o.dir)
	for _, env := range sessionEnv {
		if session := os.Getenv(env); session != "" {
			// Named so it cannot be read as an origin, which always has a
			// scheme of its own.
			b.WriteByte('\n')
			b.WriteString(env)
			b.WriteByte('=')
			b.WriteString(session)
		}
	}
	b.WriteByte('\n')
	for i, part := range parts {
		if i > 0 {
			b.WriteByte('\n')
		}
		b.WriteString(part)
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])[:keyWidth]
}
