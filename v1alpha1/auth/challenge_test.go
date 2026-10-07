package auth

import (
	"strings"
	"testing"
)

const vector = "$pbkdf2-sha256$i=600000$dHVubmVsLnBpenphL3YwMQ$UFtjhDQ2L2Fb/DQXWXQx19Nx2YTuaTLDIhGHp3Vdn24"

// TestParse pins the grammar (RFC 9110 §11.6.1) and what this tier accepts:
// Basic with a pw, optionally realm and charset; any other scheme or param is
// refused, so a typo is an error rather than a tunnel left open.
func TestParse(t *testing.T) {
	for name, tc := range map[string]struct {
		in      string
		want    int // challenges; -1 for an error
		errPart string
	}{
		"empty is public":          {"", 0, ""},
		"blank is public":          {"   ", 0, ""},
		"basic with pw":            {`Basic pw="` + vector + `"`, 1, ""},
		"realm and charset":        {`Basic realm="h", charset="UTF-8", pw="` + vector + `"`, 1, ""},
		"scheme in any case":       {`bAsIc pw="` + vector + `"`, 1, ""},
		"token value":              {`Basic pw=` + vector, 1, ""},
		"one scheme twice":         {`Basic pw="` + vector + `", Basic realm="b", pw="` + vector + `"`, -1, "twice"},
		"no pw":                    {`Basic realm="h"`, -1, "pw"},
		"wrong scheme":             {`Bearer realm="h"`, -1, "Bearer"},
		"wrong scheme twice":       {`Negotiate realm="h", Negotiate realm="i"`, -1, "not a scheme this tunnel can verify"},
		"bearer":                   {`Bearer realm="h.tunneled.pizza", sub="github:1 github:2"`, 1, ""},
		"bearer, lowercase":        {`bearer realm="h.tunneled.pizza", sub="github:1"`, 1, ""},
		"bearer with a port":       {`Bearer realm="localhost:8080", sub="github:1"`, 1, ""},
		"basic and bearer":         {`Basic pw="` + vector + `", Bearer realm="h", sub="github:1"`, 2, ""},
		"bearer, no sub":           {`Bearer realm="h"`, -1, "sub is required"},
		"bearer, no realm":         {`Bearer sub="github:1"`, -1, "realm is required"},
		"bearer, empty sub":        {`Bearer realm="h", sub=""`, -1, "lists nobody"},
		"bearer, a bare login":     {`Bearer realm="h", sub="cnuss"`, -1, "not <provider>:<id>"},
		"bearer, provider case":    {`Bearer realm="h", sub="GitHub:1"`, -1, "not <provider>:<id>"},
		"bearer, empty id":         {`Bearer realm="h", sub="github:"`, -1, "not <provider>:<id>"},
		"bearer, id too long":      {`Bearer realm="h", sub="github:` + strings.Repeat("9", 65) + `"`, -1, "not <provider>:<id>"},
		"bearer, realm not a host": {`Bearer realm="my tunnel", sub="github:1"`, -1, "not a hostname"},
		"bearer, a pw":             {`Bearer realm="h", sub="github:1", pw="` + vector + `"`, -1, `"pw" is not a parameter`},
		"bearer, too many":         {`Bearer realm="h", sub="` + strings.TrimSpace(strings.Repeat("github:1 ", 101)) + `"`, -1, "at most 100"},
		"bearer twice":             {`Bearer realm="h", sub="github:1", Bearer realm="i", sub="github:2"`, -1, "twice"},
		"invented scheme":          {`Tunneld pw="` + vector + `"`, -1, "Tunneld"},
		"unknown param":            {`Basic pw="` + vector + `", pW2="x"`, -1, "pw2"},
		"param twice":              {`Basic pw="` + vector + `", pw="` + vector + `"`, -1, "twice"},
		"param before any scheme":  {`pw="` + vector + `"`, -1, "scheme"},
		"malformed phc":            {`Basic pw="$pbkdf2-sha256$600000$x$y"`, -1, "pw"},
		"unterminated quote":       {`Basic pw="` + vector, -1, "quote"},
		"comma inside a quote":     {`Basic realm="a, b", pw="` + vector + `"`, 1, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := Parse(tc.in)
			if tc.want < 0 {
				if err == nil || !strings.Contains(err.Error(), tc.errPart) {
					t.Fatalf("Parse(%q) = %v, %v; want an error mentioning %q", tc.in, got, err, tc.errPart)
				}
				return
			}
			if err != nil || len(got) != tc.want {
				t.Fatalf("Parse(%q) = %d challenges, %v; want %d", tc.in, len(got), err, tc.want)
			}
		})
	}
	got, _ := Parse(`Basic realm="a, b", pw="` + vector + `"`)
	if got[0].Params["realm"] != "a, b" || got[0].Params["pw"] != vector {
		t.Errorf("params = %v, want realm and pw unquoted", got[0].Params)
	}
}

// TestRedacted pins what .env's reader is shown of a stored challenge: every
// parameter as given, in the order its scheme takes them, quoted again where
// it needs it, but pw's salt and hash, which are what would let a reader
// guess the password offline.
func TestRedacted(t *testing.T) {
	cs, err := Parse(`basic realm="old \"x\" example", pw="` + vector + `", charset="latin1"`)
	if err != nil {
		t.Fatal(err)
	}
	want := `Basic pw="$pbkdf2-sha256$i=600000$…$…", realm="old \"x\" example", charset="latin1"`
	if got := cs[0].Redacted(); got != want {
		t.Errorf("Redacted = %q, want %q", got, want)
	}
	if got := cs[0].Redacted(); strings.Contains(got, "dHVubmVs") || strings.Contains(got, "UFtjhDQ2") {
		t.Errorf("Redacted = %q, carries the salt or the hash", got)
	}
	cs, err = Parse(`Bearer realm="h.tunneled.pizza", sub="github:1 github:2"`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cs[0].Redacted(), `Bearer realm="h.tunneled.pizza", sub="github:1 github:2"`; got != want {
		t.Errorf("Redacted = %q, want %q: sub is the secret holder's to read", got, want)
	}
}

// TestPublic pins what a visitor is sent: by allow-list, byte for byte MDN's
// Basic example, the realm as stored (quoted again), none when none was
// stored, and never pw.
func TestPublic(t *testing.T) {
	for name, tc := range map[string]struct{ value, want string }{
		"the stored realm": {`basic realm="old.example", pw="` + vector + `", charset="latin1"`, `Basic realm="old.example", charset="UTF-8"`},
		"no realm stored":  {`Basic pw="` + vector + `"`, `Basic charset="UTF-8"`},
		"quoted again":     {`Basic realm="a \\ \"b\"", pw="` + vector + `"`, `Basic realm="a \\ \"b\"", charset="UTF-8"`},
		"bearer": {`Bearer realm="h.tunneled.pizza", sub="github:1"`,
			`Bearer realm="h.tunneled.pizza", resource_metadata="https://h.tunneled.pizza/.well-known/oauth-protected-resource", scope="openid profile"`},
	} {
		t.Run(name, func(t *testing.T) {
			cs, err := Parse(tc.value)
			if err != nil {
				t.Fatal(err)
			}
			got := cs[0].Public()
			if got != tc.want {
				t.Errorf("Public = %q, want %q", got, tc.want)
			}
			if strings.Contains(got, "pbkdf2") {
				t.Error("pw leaked into the public form")
			}
			if strings.Contains(got, "github:") || strings.Contains(got, "sub=") {
				t.Error("sub leaked into the public form")
			}
		})
	}
}

// TestPublicFor pins the gate's 401: the resource_metadata of the host the
// visitor asked for, whatever realm is stored; Basic is unchanged by it.
func TestPublicFor(t *testing.T) {
	cs, err := Parse(`Basic realm="stored", pw="` + vector + `", Bearer realm="stored.example", sub="github:1"`)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := cs[0].publicFor("asked.example"), `Basic realm="stored", charset="UTF-8"`; got != want {
		t.Errorf("Basic publicFor = %q, want %q", got, want)
	}
	want := `Bearer realm="stored.example", resource_metadata="https://asked.example/.well-known/oauth-protected-resource", scope="openid profile"`
	if got := cs[1].publicFor("asked.example"); got != want {
		t.Errorf("Bearer publicFor = %q, want %q", got, want)
	}
	if got, want := cs[1].publicFor(""), `Bearer realm="stored.example", scope="openid profile"`; got != want {
		t.Errorf("Bearer publicFor no host = %q, want %q", got, want)
	}
}
