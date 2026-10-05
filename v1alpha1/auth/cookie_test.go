package auth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"
	"time"
)

func TestCookie(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	key := cookieKey([]byte("s3cr3t"))
	const value = `Basic pw="x"`
	tok := mintCookie(key, "basic", value, now)
	if !strings.HasPrefix(tok, "v1.") || strings.Count(tok, ".") != 2 {
		t.Fatalf("token %q, want v1.<payload>.<mac>", tok)
	}
	for name, tc := range map[string]struct {
		key          []byte
		token, value string
		schemes      []string
		at           time.Time
		want         bool
	}{
		"valid":                  {key, tok, value, []string{"basic"}, now, true},
		"a day before expiry":    {key, tok, value, []string{"basic"}, now.Add(29 * 24 * time.Hour), true},
		"expired":                {key, tok, value, []string{"basic"}, now.Add(31 * 24 * time.Hour), false},
		"value changed":          {key, tok, `Basic pw="y"`, []string{"basic"}, now, false},
		"scheme gone":            {key, tok, value, []string{"bearer"}, now, false},
		"new secret":             {cookieKey([]byte("other")), tok, value, []string{"basic"}, now, false},
		"unknown version":        {key, "v2" + tok[2:], value, []string{"basic"}, now, false},
		"tampered payload":       {key, tamper(tok), value, []string{"basic"}, now, false},
		"no key (no secret yet)": {nil, tok, value, []string{"basic"}, now, false},
		"garbage":                {key, "nope", value, []string{"basic"}, now, false},
	} {
		t.Run(name, func(t *testing.T) {
			if got := readCookie(tc.key, tc.token, tc.value, tc.schemes, tc.at); got != tc.want {
				t.Errorf("readCookie = %v, want %v", got, tc.want)
			}
		})
	}

	// A MAC keyed with the raw secret, the control path's token, never passes:
	// one key for two jobs would let an answer meant for one serve the other.
	payload := strings.Split(tok, ".")[1]
	m := hmac.New(sha256.New, []byte("s3cr3t"))
	m.Write([]byte("v1." + payload + "\n" + value))
	raw := "v1." + payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if readCookie(key, raw, value, []string{"basic"}, now) {
		t.Error("a cookie keyed with the raw secret verified")
	}
	if cookieKey(nil) != nil {
		t.Error("an empty secret derived a key")
	}
}

func tamper(tok string) string {
	parts := strings.Split(tok, ".")
	b, _ := base64.RawURLEncoding.DecodeString(parts[1])
	b = []byte(strings.Replace(string(b), `"s":"basic"`, `"s":"bAsic"`, 1))
	return parts[0] + "." + base64.RawURLEncoding.EncodeToString(b) + "." + parts[2]
}
