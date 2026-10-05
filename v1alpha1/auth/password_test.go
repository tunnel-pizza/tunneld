package auth

import (
	"strings"
	"testing"

	"golang.org/x/text/unicode/norm"
)

// nfd is the interop vector's password typed decomposed: "ü" as u + U+0308.
// tunnel.pizza's password.test.ts asserts the same vector; if the two sides
// ever disagree on the format or the normalization, one goes red.
const nfd = "Pizza für alle \U0001F355"

func TestInteropVector(t *testing.T) {
	p, err := parsePHC(vector)
	if err != nil {
		t.Fatal(err)
	}
	if !p.verify(nfd) {
		t.Error("the NFD spelling does not verify")
	}
	if !p.verify(norm.NFC.String(nfd)) {
		t.Error("the NFC spelling does not verify")
	}
	if p.verify("Pizza fur alle 🍕") || p.verify("") {
		t.Error("a wrong or empty password verified")
	}
}

func TestParsePHC(t *testing.T) {
	salt16 := "dHVubmVsLnBpenphL3YwMQ"                      // 16 bytes
	hash32 := "UFtjhDQ2L2Fb/DQXWXQx19Nx2YTuaTLDIhGHp3Vdn24" // 32 bytes
	for name, tc := range map[string]struct {
		in, errPart string
	}{
		"the vector":         {vector, ""},
		"iterations low":     {"$pbkdf2-sha256$i=99999$" + salt16 + "$" + hash32, "iterations"},
		"iterations high":    {"$pbkdf2-sha256$i=2000001$" + salt16 + "$" + hash32, "iterations"},
		"iterations not int": {"$pbkdf2-sha256$i=x$" + salt16 + "$" + hash32, "iterations"},
		"short salt":         {"$pbkdf2-sha256$i=600000$c2hvcnQ$" + hash32, "salt"},
		"hash not 32":        {"$pbkdf2-sha256$i=600000$" + salt16 + "$c2hvcnQ", "hash"},
		"padded base64":      {"$pbkdf2-sha256$i=600000$" + salt16 + "==$" + hash32, "salt"},
		"other algorithm":    {"$scrypt$ln=15$" + salt16 + "$" + hash32, "pbkdf2-sha256"},
		"missing part":       {"$pbkdf2-sha256$i=600000$" + salt16, "pbkdf2-sha256"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := parsePHC(tc.in)
			if tc.errPart == "" && err != nil || tc.errPart != "" && (err == nil || !strings.Contains(err.Error(), tc.errPart)) {
				t.Errorf("parsePHC(%q) = %v, want error containing %q", tc.in, err, tc.errPart)
			}
		})
	}
}
