// Package auth is what stands between a visitor and everything a tunnel
// serves: the password that TUNNELD_WWW_AUTHENTICATE names, the login page and
// cookie a browser gets, and the bodyless 401 an API client gets.
//
// The variable is a WWW-Authenticate field value (RFC 9110 §11.6.1) whose
// challenges carry their own verifiers as private parameters:
//
//	Basic realm="0t8qsb6pq3.tunneled.pizza", pw="$pbkdf2-sha256$i=600000$…$…"
//
// Parsed generically, then handed to a verifier by scheme, so a later tier
// (Bearer) is a verifier and a row in the allow-list, not a new parser.
package auth

import (
	"errors"
	"fmt"
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
// an invented one: Basic today; Bearer is a later tier.
var schemes = map[string]schemeRule{
	"basic": {
		name:     "Basic",
		required: []string{"pw"},
		allowed:  []string{"pw", "realm", "charset"},
		check:    func(c Challenge) error { _, err := parsePHC(c.Params["pw"]); return err },
	},
}

func (c Challenge) scheme() string { return strings.ToLower(c.Scheme) }

// Public is the challenge as a visitor may see it: the canonical scheme, the
// realm set to host (left out when host is ""), and charset="UTF-8" for Basic
// (RFC 7617). Nothing else, pw least of all.
func (c Challenge) Public(host string) string {
	rule := schemes[c.scheme()]
	var params []string
	if host != "" {
		params = append(params, `realm="`+host+`"`)
	}
	if rule.name == "Basic" {
		params = append(params, `charset="UTF-8"`)
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
			return nil, fmt.Errorf("%s: not a scheme this tunnel can verify (Basic is)", c.Scheme)
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

// parsePHC is replaced in the next commit.
func parsePHC(s string) (phc, error) {
	if !strings.HasPrefix(s, "$pbkdf2-sha256$i=") || strings.Count(s, "$") != 4 {
		return phc{}, errors.New("pw is not a $pbkdf2-sha256$i=…$…$… string")
	}
	return phc{}, nil
}

type phc struct{}
