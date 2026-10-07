// Package auth is what stands between a visitor and everything a tunnel
// serves: the password that TUNNELD_WWW_AUTHENTICATE names, the login page and
// cookie a browser gets, and the bodyless 401 an API client gets.
//
// The variable is a WWW-Authenticate field value (RFC 9110 §11.6.1) whose
// challenges carry their own verifiers as private parameters:
//
//	Basic realm="0t8qsb6pq3.tunneled.pizza", pw="$pbkdf2-sha256$i=600000$…$…"
//
// Parsed generically, then handed to a verifier by scheme: Basic carries a
// password's hash, Bearer the list of who may sign in through the provider.
package auth

import (
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

// Challenge is one auth-scheme and its parameters, as the variable holds it:
// Scheme as written, Params keyed in lowercase with their values unquoted.
type Challenge struct {
	Scheme string
	Params map[string]string
}

// schemeRule is what one scheme takes on input and how it shows itself. What
// goes out is built from an allow-list (Public), not by stripping: a secret
// parameter added later cannot leak by being forgotten.
type schemeRule struct {
	name     string   // canonical spelling, as it goes out
	required []string // input params that must be present
	allowed  []string // every input param it takes
	check    func(Challenge) error
}

// schemes is every scheme with a verifier. Only IANA-registered schemes, never
// an invented one.
var schemes = map[string]schemeRule{
	"basic": {
		name:     "Basic",
		required: []string{"pw"},
		allowed:  []string{"pw", "realm", "charset"},
		check:    func(c Challenge) error { _, err := parsePHC(c.Params["pw"]); return err },
	},
	"bearer": {
		name:     "Bearer",
		required: []string{"realm", "sub"},
		allowed:  []string{"realm", "sub"},
		check:    checkBearer,
	},
}

// maxSubs bounds a Bearer's list: every entry is compared on every request.
const maxSubs = 100

// hostname is what a Host header holds: labels and dots, an optional port.
var hostname = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]*[A-Za-z0-9])?(:[0-9]{1,5})?$`)

// checkBearer is Bearer's rule: realm is the tunnel's hostname, since the
// public form builds resource_metadata from it, and sub lists who may enter.
func checkBearer(c Challenge) error {
	if !hostname.MatchString(c.Params["realm"]) {
		return fmt.Errorf("realm %q is not a hostname", c.Params["realm"])
	}
	_, err := parseSubs(c.Params["sub"])
	return err
}

// parseSubs is a Bearer's sub: space-separated <provider>:<id>, the provider
// lowercase letters, the id 1 to 64 visible ASCII characters, neither quote
// nor backslash.
func parseSubs(s string) ([]string, error) {
	subs := strings.Fields(s)
	if len(subs) == 0 {
		return nil, errors.New("sub lists nobody")
	}
	if len(subs) > maxSubs {
		return nil, fmt.Errorf("sub lists %d, at most %d", len(subs), maxSubs)
	}
	for _, sub := range subs {
		provider, id, _ := strings.Cut(sub, ":")
		if provider == "" || strings.ContainsFunc(provider, func(r rune) bool { return r < 'a' || r > 'z' }) ||
			id == "" || len(id) > 64 || strings.ContainsFunc(id, func(r rune) bool { return r < 0x21 || r > 0x7e || r == '"' || r == '\\' }) {
			return nil, fmt.Errorf("sub %q is not <provider>:<id>", sub)
		}
	}
	return subs, nil
}

func (c Challenge) scheme() string { return strings.ToLower(c.Scheme) }

// Public is the challenge as a visitor may see it, the host it names being
// its stored realm: see publicFor.
func (c Challenge) Public() string { return c.publicFor(c.Params["realm"]) }

// publicFor is the challenge as a visitor of host may see it: the canonical
// scheme and the realm as stored (left out when none was); for Basic,
// charset="UTF-8" (RFC 7617); for Bearer, host's resource_metadata (RFC 9728
// §5.1, left out when host is "") and the scopes to ask for (RFC 6750 §3).
// Nothing else, pw and sub least of all.
func (c Challenge) publicFor(host string) string {
	rule := schemes[c.scheme()]
	var params []string
	if realm, ok := c.Params["realm"]; ok {
		params = append(params, `realm="`+quoted.Replace(realm)+`"`)
	}
	switch rule.name {
	case "Basic":
		params = append(params, `charset="UTF-8"`)
	case "Bearer":
		if host != "" {
			params = append(params, `resource_metadata="https://`+quoted.Replace(host)+MetadataPath+`"`)
		}
		params = append(params, `scope="openid profile"`)
	}
	if len(params) == 0 {
		return rule.name
	}
	return rule.name + " " + strings.Join(params, ", ")
}

// withRealm is c with its realm set to realm, its Params copied rather than
// shared, so the stored challenge is left as it was.
func (c Challenge) withRealm(realm string) Challenge {
	c.Params = maps.Clone(c.Params)
	c.Params["realm"] = realm
	return c
}

// quoted escapes a parameter's value for a quoted-string (RFC 9110 §5.6.4).
var quoted = strings.NewReplacer(`\`, `\\`, `"`, `\"`)

// Redacted is the challenge as stored, for whoever reads .env: every
// parameter it was given, in the order its scheme takes them, but pw's salt
// and hash, which are what would let a reader guess the password offline.
func (c Challenge) Redacted() string {
	rule := schemes[c.scheme()]
	var params []string
	for _, key := range rule.allowed {
		v, ok := c.Params[key]
		if !ok {
			continue
		}
		if key == "pw" {
			v = "…"
			if p, err := parsePHC(c.Params[key]); err == nil {
				v = fmt.Sprintf("$pbkdf2-sha256$i=%d$…$…", p.iter)
			}
		}
		params = append(params, key+`="`+quoted.Replace(v)+`"`)
	}
	if len(params) == 0 {
		return rule.name
	}
	return rule.name + " " + strings.Join(params, ", ")
}

// Parse reads value as challenges and checks each against its scheme's rule.
// "" (or blank) is public: nil, nil.
func Parse(value string) ([]Challenge, error) {
	items, err := splitList(value)
	if err != nil {
		return nil, err
	}
	var out []Challenge
	for _, item := range items {
		if key, val, ok := param(item); ok {
			if len(out) == 0 {
				return nil, fmt.Errorf("%q: a parameter before any scheme", key)
			}
			last := &out[len(out)-1]
			if _, twice := last.Params[key]; twice {
				return nil, fmt.Errorf("%s: %q given twice", last.Scheme, key)
			}
			last.Params[key] = val
			continue
		}
		scheme, rest, _ := strings.Cut(item, " ")
		if !isToken(scheme) {
			return nil, fmt.Errorf("%q is not a scheme", scheme)
		}
		c := Challenge{Scheme: scheme, Params: map[string]string{}}
		if rest = strings.TrimSpace(rest); rest != "" {
			key, val, ok := param(rest)
			if !ok {
				return nil, fmt.Errorf("%s: %q is not a parameter (token68 is not taken)", scheme, rest)
			}
			c.Params[key] = val
		}
		out = append(out, c)
	}
	for _, c := range out {
		rule, ok := schemes[c.scheme()]
		if !ok {
			return nil, fmt.Errorf("%s: not a scheme this tunnel can verify (Basic and Bearer are)", c.Scheme)
		}
		for key := range c.Params {
			if !slices.Contains(rule.allowed, key) {
				return nil, fmt.Errorf("%s: %q is not a parameter it takes", rule.name, key)
			}
		}
		for _, key := range rule.required {
			if _, ok := c.Params[key]; !ok {
				return nil, fmt.Errorf("%s: %s is required", rule.name, key)
			}
		}
		if err := rule.check(c); err != nil {
			return nil, fmt.Errorf("%s: %w", rule.name, err)
		}
	}
	// One challenge per scheme: each pw is a PBKDF2 run per wrong password,
	// so a value holding many would multiply what one guess costs the run.
	seen := map[string]bool{}
	for _, c := range out {
		if seen[c.scheme()] {
			return nil, fmt.Errorf("%s: given twice (one challenge per scheme)", c.Scheme)
		}
		seen[c.scheme()] = true
	}
	return out, nil
}

// splitList splits on commas outside quoted strings, trimming each item and
// dropping empty ones (RFC 9110 §5.6.1 allows them).
func splitList(s string) ([]string, error) {
	var items []string
	var cur strings.Builder
	quoted, escaped := false, false
	for _, r := range s {
		switch {
		case escaped:
			escaped = false
		case quoted && r == '\\':
			escaped = true
		case r == '"':
			quoted = !quoted
		case r == ',' && !quoted:
			items = append(items, strings.TrimSpace(cur.String()))
			cur.Reset()
			continue
		}
		cur.WriteRune(r)
	}
	if quoted {
		return nil, errors.New("an unterminated quote")
	}
	items = append(items, strings.TrimSpace(cur.String()))
	out := items[:0]
	for _, it := range items {
		if it != "" {
			out = append(out, it)
		}
	}
	return out, nil
}

// param reads "key=value" or `key="quoted value"`, key lowercased. ok is false
// for anything else, a scheme with its first parameter among them, since that
// has a space before its "=".
func param(item string) (key, val string, ok bool) {
	k, v, found := strings.Cut(item, "=")
	k = strings.TrimSpace(k)
	if !found || !isToken(k) {
		return "", "", false
	}
	v = strings.TrimSpace(v)
	if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
		v = unescape(v[1 : len(v)-1])
	}
	return strings.ToLower(k), v, true
}

// unescape takes the backslashes off a quoted-string's quoted-pairs.
func unescape(s string) string {
	var b strings.Builder
	escaped := false
	for _, r := range s {
		if !escaped && r == '\\' {
			escaped = true
			continue
		}
		escaped = false
		b.WriteRune(r)
	}
	return b.String()
}

// isToken reports whether s is an RFC 9110 token.
func isToken(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if r > 127 || !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", r)) {
			return false
		}
	}
	return true
}
