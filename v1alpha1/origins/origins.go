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
	"net"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"

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

// WithOrigins adds origins, in order, appending across calls — so a list can
// be assembled from more than one source without the caller joining them
// first. Each is numbered by the place it lands, as Add numbers it. A repeat
// costs nothing: Key deduplicates, and the run exposes what it was given.
func WithOrigins(origins ...v1.Origin) Option {
	return func(o *OriginsImpl) { o.Add(origins...) }
}

// NewOrigin is u as an origin, worked out once: what it counts as, who it is
// asked of and what for. Its Index is 0 until a list numbers it.
//
// The reference is the path, and the authority decides how much of it: with
// a provider named the leading separator belongs to the URL and not to the
// reference (attach://dockerd/api is the container api), and without one the
// empty authority is this machine, so the path is a filesystem path and keeps
// every byte (exec:///usr/bin/top). An address's reference is its host and
// port, with its path but for a bare "/".
func NewOrigin(u *url.URL) v1.Origin {
	o := v1.Origin{URI: *u, Scheme: v1.HttpScheme}
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

// servedSchemes is every scheme whose value names something tunneld serves on
// the origin's behalf rather than an address it proxies to. The binder stands
// a loopback attach server up for each one, and the provider behind that
// scheme is what resolves the reference.
//
// The words are for the operator: a message that says "names no container" for
// attach:// and "names no program" for exec:// is one somebody can act on,
// where a message about a malformed authority component is not. A scheme
// absent from here is not served, which is what makes the parser and the
// binder agree on the same list.
var servedSchemes = map[v1.Scheme]struct {
	noun, example string
	// resolve, when set, is how an authority with nothing after it is read
	// before it is read as a provider: exec://htop is the program, because a
	// provider with no reference could not be asked for anything anyway, so
	// the reading that can succeed wins over the one that cannot.
	//
	// It is the same function the bare-argument shorthand uses, held here
	// rather than called by name below so the two readings of a word cannot
	// drift — whatever `tunneld htop` accepts, exec://htop accepts.
	resolve func(string) (string, bool)
}{
	v1.AttachScheme: {noun: "container", example: "attach://dockerd/my-container"},
	v1.ExecScheme:   {noun: "program", example: "exec:///usr/bin/htop", resolve: Resolve},
}

// Resolve answers whether s names a program this machine can run, and with
// what: the absolute path of the thing that would actually run.
//
// The path rather than a bare yes, because the origin carries it from here on.
// "top" says which program only on the machine that resolved it, while
// /usr/bin/top says it anywhere — so that is what the frame puts in its corner,
// what the reported map prints, and what somebody pastes back into a command
// line to get the same program rather than whatever their own $PATH finds.
//
// exec.LookPath is the whole of the lookup, which means the rules are the
// host's rather than ours: the $PATH search and the executable bit on Unix,
// the PATHEXT extensions on Windows, and a value carrying a separator taken as
// a path to the file itself rather than as a name to search for. Anything
// shaped like a URL answers false for that last reason — "http://localhost:3000"
// is stat'd as a path and is not there.
//
// A command that resolves only through the working directory answers false
// too: LookPath returns exec.ErrDot for it, and a file that happens to sit
// where tunneld was started should not become a public origin because somebody
// typed its name.
//
// The answer is about this machine and this moment. The same argument is a
// program on one host and a hostname on another, which is why nothing here is
// cached and why the lookup happens while origins are settled rather than when
// they are seeded.
func Resolve(s string) (string, bool) {
	path, err := exec.LookPath(s)
	if err != nil {
		return "", false
	}
	// LookPath answers a PATH hit absolutely and a path-shaped argument as it
	// was given, so this is what makes the two agree.
	//
	// On Windows the absolute form is C:\..., which has no spelling inside a
	// exec:// URL — whichever half of the URL it is put in, url.URL escapes
	// the separators. The origin is still correct where it counts, since the
	// binder reads the path off the URL rather than off its printed form, and
	// the platform has no pseudo-terminals to serve it on either way.
	abs, err := filepath.Abs(path)
	if err != nil {
		return path, true
	}
	return abs, true
}

// program is a program origin with args as its arguments: the words after it
// on the command line, carried as the origin's query under v1.ArgKey so they
// arrive the same way from argv, the environment and a seed, and so the frame
// shows a string that pastes back into a command line as the same run.
//
// Appended to any the origin already carries, in order — a seed can name a
// program's first arguments and the command line its last.
//
// This is where the command line stops being a list of origins: the words
// after a program are the program's, so it is the last origin on the line,
// the way docker's image is followed only by its own command. A second
// program, or a URL after the first, is that program's argument.
func program(u *url.URL, args []string) *url.URL {
	if len(args) == 0 {
		return u
	}
	q := u.Query()
	for _, arg := range args {
		q.Add(v1.ArgKey, arg)
	}
	u.RawQuery = q.Encode()
	return u
}

// split divides the words after a program into its arguments and what comes
// after them: the arguments run until the first word that can only be an
// origin, and that word and the rest are origins again.
//
// The docker-run rule — a program takes every word after it — is what lets
// `tunneld claude --resume` work without a separator, and it is kept. But
// ":8000" is not an argument anybody means: a port with nothing in front of it
// is the shorthand every other position reads as this machine's :8000, and a
// URL with a scheme is an address. Either one after a program is a second
// origin, so `tunneld bash :8000` is a shell beside a service rather than a
// shell told to run a script called :8000. So is the program's own word said
// again: `tunneld bash bash` is two shells, not a shell looking for a script
// called bash — nobody passes a program its own name as an argument on
// purpose, and two of the same terminal is the ordinary way to want a second
// one. A flag, a path, any other bare word — a program's, as before.
func split(word string, words []string) (args, rest []string) {
	for i, w := range words {
		if isOrigin(w) || w == word {
			return words[:i], words[i:]
		}
	}
	return words, nil
}

// fields splits one word into the words a shell would have made of it had the
// outer quotes not been there: whitespace separates, single quotes keep
// everything literal, double quotes keep everything but a backslash escape,
// and a backslash outside quotes keeps the next character. An unclosed quote
// runs to the end rather than failing — the word already reached us, and
// what it meant is clearer kept than dropped.
func fields(word string) []string {
	var (
		out   []string
		cur   strings.Builder
		open  bool // something has gone into cur, even if empty ("")
		quote rune // 0, '\'', or '"'
	)
	flush := func() {
		if open {
			out = append(out, cur.String())
			cur.Reset()
			open = false
		}
	}
	rs := []rune(word)
	for i := 0; i < len(rs); i++ {
		r := rs[i]
		switch {
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			if r == '"' {
				quote = 0
			} else if r == '\\' && i+1 < len(rs) && strings.ContainsRune("\"\\$`", rs[i+1]) {
				i++
				cur.WriteRune(rs[i])
			} else {
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote, open = r, true
		case r == '\\' && i+1 < len(rs):
			i++
			cur.WriteRune(rs[i])
			open = true
		case unicode.IsSpace(r):
			flush()
		default:
			cur.WriteRune(r)
			open = true
		}
	}
	flush()
	return out
}

// isOrigin reports whether a word can only be an origin: a port shorthand
// (":8000") or a URL with a scheme ("http://…", "attach://…"). Anything else
// after a program is the program's.
func isOrigin(word string) bool {
	if port, ok := strings.CutPrefix(word, ":"); ok {
		_, err := strconv.Atoi(port)
		return err == nil && port != ""
	}
	return strings.Contains(word, "://")
}

// arguments reports whether a served origin's query is a program's arguments
// and nothing else — the one query a served origin is allowed to carry.
func arguments(u *url.URL) bool {
	if v1.Scheme(u.Scheme) != v1.ExecScheme {
		return false
	}
	for key := range u.Query() {
		if key != v1.ArgKey {
			return false
		}
	}
	return true
}

// Parse is settled read as origins: each word a program on this machine, a
// program with its arguments, or an address. A word it cannot use is dropped
// with a warning rather than failing the run.
func Parse(log v1.Logger, settled []string) *OriginsImpl {
	parsed := make([]v1.Origin, 0, len(settled))
	// One line per origin at the moment it is settled, because how the words
	// were read is the thing the report cannot show: a program's arguments
	// hide behind its label, and a word taken as an argument looks exactly
	// like a word that was never there.
	add := func(word string, u *url.URL) {
		log.Info(fmt.Sprintf("%q interpolated into %s", word, u))
		parsed = append(parsed, NewOrigin(u))
	}
	// The first origin seen carrying a +ws marker, kept to strip a second, and
	// its index in the list, which is where the marker goes once it is off the
	// scheme.
	wsOrigin, ws := "", -1
	for i := 0; i < len(settled); i++ {
		s := strings.TrimSpace(settled[i])
		word := s // as typed, for the log; s is rewritten on the way to a URL
		if s == "" {
			log.Warn("dropping an origin", "origin", s, "reason", "empty, pass a local service URL (e.g. http://localhost:3000)")
			continue
		}
		// Short circuit for executables: a path to a binary is not a URL, and the binder runs it
		if path, ok := Resolve(s); ok {
			// Built rather than parsed, and done with: the reference resolved
			// a line ago, so there is nothing for the checks below to add.
			//
			// The resolved path rather than the word typed, because the origin
			// is what everything downstream shows and what somebody pastes
			// back — "top" names a program only on the machine that looked it
			// up. It goes in Path and not Host because that is the only shape
			// an absolute path survives: url.URL escapes the separators of a
			// host, so exec:///usr/bin/top round-trips and exec://%2Fusr%2Fbin
			// is what the other spelling produces. The empty authority is this
			// machine, which is the whole of what exec:// with no provider says.
			args, _ := split(s, settled[i+1:])
			add(word, program(&url.URL{Scheme: string(v1.ExecScheme), Path: path}, args))
			i += len(args)
			continue
		}
		// A word with spaces in it is a program and its arguments quoted as
		// one thing — no origin can contain whitespace, and the shell has
		// already taken the quotes off. Split the way a shell would, so an
		// argument that was itself quoted stays whole. The group is complete:
		// the space inside the word is the one piece of quoting that survives
		// the shell, and it says the person closed the group where they did,
		// so the words after it on the line are origins again and a bare
		// `bash` after `'next dev'` is a second terminal, not next's third
		// argument. Quoting is also how a program with arguments reads inside
		// a comma-separated TUNNELD_ORIGINS.
		if fs := fields(s); len(fs) > 1 {
			if path, ok := Resolve(fs[0]); ok {
				add(word, program(&url.URL{Scheme: string(v1.ExecScheme), Path: path}, fs[1:]))
				continue
			}
			log.Warn("dropping an origin", "origin", s, "reason", "has spaces but "+fs[0]+" names no program that can be run")
			continue
		}
		if !strings.Contains(s, "://") {
			s = "http://" + s
		}
		u, err := url.Parse(s)
		if err != nil {
			log.Warn("dropping an origin", "origin", s, "reason", "not a URL", "err", err)
			continue
		}
		// A served origin is not proxied at all: it names a thing the binder
		// stands a loopback server up for — a container, a program — rather
		// than an address to reach. Everything the shorthands below fill in — a
		// default scheme, a default host, a preserved path — is meaningless
		// here, so the value is taken exactly as typed and anything extra is
		// dropped rather than silently ignored.
		if what, served := servedSchemes[v1.Scheme(u.Scheme)]; served {
			// An authority with nothing after it is not a provider being asked
			// for something — there is nothing to ask for — so where the
			// scheme allows it, the word is looked up as a program first. It
			// is rewritten to the resolved path for the same reason the bare
			// argument above is: the word names a program only on the machine
			// that looked it up.
			if what.resolve != nil && u.Host != "" && u.Path == "" {
				if path, ok := what.resolve(u.Host); ok {
					args, _ := split(s, settled[i+1:])
					add(word, program(&url.URL{Scheme: u.Scheme, Path: path}, args))
					i += len(args)
					continue
				}
			}
			// The reference is the path. The authority beside it names the
			// provider that resolves the reference, and an empty one is this
			// machine — which is why the binder, not this loop, decides how
			// much of the path is the reference: with a provider the leading
			// separator is the URL's, and without one the path is absolute and
			// keeps every byte. Here only its emptiness matters.
			if strings.TrimPrefix(u.Path, "/") == "" {
				log.Warn("dropping an origin", "origin", s, "reason", "names no "+what.noun+", pass e.g. "+what.example)
				continue
			}
			// A reference and nothing else: a fragment or a userinfo is not
			// part of the name of anything served, and neither is a query —
			// except a program's arguments, which are the one thing a program
			// origin carries besides the program.
			if u.Fragment != "" || u.User != nil || (u.RawQuery != "" && !arguments(u)) {
				log.Warn("dropping an origin", "origin", s, "reason", "carries more than a "+what.noun+" reference, pass "+u.Scheme+"://"+u.Host+u.Path)
				continue
			}
			if v1.Scheme(u.Scheme) == v1.ExecScheme {
				args, _ := split(s, settled[i+1:])
				add(word, program(u, args))
				i += len(args)
				continue
			}
			add(word, u)
			continue
		}
		// A +ws / +wss suffix declares that this origin owns WebSockets, so a
		// handshake the page did not build with a routing index goes here
		// rather than following the sticky cookie. The marker comes off here
		// and rides the list as an index (WithWebSocket), so every URL
		// downstream — the binder's, the panel's, the one the router dials —
		// is the bare address and nothing has to know to leave it alone
		// (#173, #176). The key follows: an origin marked and unmarked is one
		// tunnel.
		//
		// The two spellings (+ws and +wss) mean the same thing: the suffix
		// induces the designation rather than describing a transport — the
		// origin is dialed by its base scheme either way — so accepting both
		// spares an operator from reasoning about which one their service
		// "is", which is not a question the marker asks.
		base, marked := u.Scheme, false
		if s, ok := strings.CutSuffix(u.Scheme, "+wss"); ok {
			base, marked = s, true
		} else if s, ok := strings.CutSuffix(u.Scheme, "+ws"); ok {
			base, marked = s, true
		}
		if base != "http" && base != "https" {
			log.Warn("dropping an origin", "origin", s, "reason", "scheme is not http, https, "+v1.AttachScheme+" or "+v1.ExecScheme, "scheme", u.Scheme)
			continue
		}
		u.Scheme = base
		// A port with no host in front of it — ":8000", or the "http://:8000"
		// the scheme default above makes of it — means the local machine, the
		// way every dev server reads that shorthand. The test is Hostname, not
		// Host: url.Parse keeps the colon, so ":8000" arrives as a non-empty
		// Host with nothing before the port, and a bare Host check waves it
		// through as the unresolvable origin "http://:8000".
		if u.Hostname() == "" {
			if u.Port() == "" {
				log.Warn("dropping an origin", "origin", s, "reason", "no host, pass e.g. http://localhost:3000")
				continue
			}
			u.Host = net.JoinHostPort("localhost", u.Port())
		}
		// Claimed only here, once the origin is certain to be kept: an origin
		// dropped above for want of a host would otherwise hold the claim and
		// strip the marker from the one that should have had it.
		if marked {
			if wsOrigin != "" {
				// Two origins cannot both own the websockets. The marker goes
				// and the origin stays: it is a perfectly good origin that
				// asked for something already taken, and a handshake has
				// only one place to go.
				log.Warn("dropping a websockets marker", "origin", s, "reason", "already claimed by "+wsOrigin+", mark only one", "scheme", base)
			} else {
				wsOrigin, ws = s, len(parsed)
			}
		}
		add(word, u)
	}
	return New(WithOrigins(parsed...), WithWebSocket(ws))
}
