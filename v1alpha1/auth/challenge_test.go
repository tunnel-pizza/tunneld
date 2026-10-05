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
		"empty is public":         {"", 0, ""},
		"blank is public":         {"   ", 0, ""},
		"basic with pw":           {`Basic pw="` + vector + `"`, 1, ""},
		"realm and charset":       {`Basic realm="h", charset="UTF-8", pw="` + vector + `"`, 1, ""},
		"scheme in any case":      {`bAsIc pw="` + vector + `"`, 1, ""},
		"token value":             {`Basic pw=` + vector, 1, ""},
		"one scheme twice":        {`Basic pw="` + vector + `", Basic realm="b", pw="` + vector + `"`, -1, "twice"},
		"no pw":                   {`Basic realm="h"`, -1, "pw"},
		"wrong scheme":            {`Bearer realm="h"`, -1, "Bearer"},
		"wrong scheme twice":      {`Bearer realm="h", Bearer realm="i"`, -1, "not a scheme this tunnel can verify"},
		"invented scheme":         {`Tunneld pw="` + vector + `"`, -1, "Tunneld"},
		"unknown param":           {`Basic pw="` + vector + `", pW2="x"`, -1, "pw2"},
		"param twice":             {`Basic pw="` + vector + `", pw="` + vector + `"`, -1, "twice"},
		"param before any scheme": {`pw="` + vector + `"`, -1, "scheme"},
		"malformed phc":           {`Basic pw="$pbkdf2-sha256$600000$x$y"`, -1, "pw"},
		"unterminated quote":      {`Basic pw="` + vector, -1, "quote"},
		"comma inside a quote":    {`Basic realm="a, b", pw="` + vector + `"`, 1, ""},
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

// TestPublic pins what a visitor is sent: by allow-list, byte for byte MDN's
// Basic example, realm from the request's host whatever was stored, and never
// pw; an empty host leaves the realm out.
func TestPublic(t *testing.T) {
	cs, err := Parse(`basic realm="old.example", pw="` + vector + `", charset="latin1"`)
	if err != nil {
		t.Fatal(err)
	}
	if got := cs[0].Public("0t8qsb6pq3.tunneled.pizza"); got != `Basic realm="0t8qsb6pq3.tunneled.pizza", charset="UTF-8"` {
		t.Errorf("Public = %q", got)
	}
	if got := cs[0].Public(""); got != `Basic charset="UTF-8"` {
		t.Errorf(`Public("") = %q, want the realm left out`, got)
	}
	if strings.Contains(cs[0].Public("h"), "pbkdf2") {
		t.Error("pw leaked into the public form")
	}
}
