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
	tok := mintCookie(key, "basic", "", value, now)
	if !strings.HasPrefix(tok, "v1.") || strings.Count(tok, ".") != 2 {
		t.Fatalf("token %q, want v1.<payload>.<mac>", tok)
	}
	sso := mintCookie(key, "bearer", "github:1", value, now)
	for name, tc := range map[string]struct {
		key           []byte
		token, value  string
		schemes, subs []string
		at            time.Time
		want          bool
		wantSub       string
	}{
		"valid":                  {key, tok, value, []string{"basic"}, nil, now, true, ""},
		"a day before expiry":    {key, tok, value, []string{"basic"}, nil, now.Add(29 * 24 * time.Hour), true, ""},
		"expired":                {key, tok, value, []string{"basic"}, nil, now.Add(31 * 24 * time.Hour), false, ""},
		"value changed":          {key, tok, `Basic pw="y"`, []string{"basic"}, nil, now, false, ""},
		"scheme gone":            {key, tok, value, []string{"bearer"}, nil, now, false, ""},
		"new secret":             {cookieKey([]byte("other")), tok, value, []string{"basic"}, nil, now, false, ""},
		"unknown version":        {key, "v2" + tok[2:], value, []string{"basic"}, nil, now, false, ""},
		"tampered payload":       {key, tamper(tok), value, []string{"basic"}, nil, now, false, ""},
		"no key (no secret yet)": {nil, tok, value, []string{"basic"}, nil, now, false, ""},
		"garbage":                {key, "nope", value, []string{"basic"}, nil, now, false, ""},
		"sso, listed":            {key, sso, value, []string{"bearer"}, []string{"github:2", "github:1"}, now, true, "github:1"},
		"sso, no longer listed":  {key, sso, value, []string{"bearer"}, []string{"github:2"}, now, false, ""},
		"sso, scheme gone":       {key, sso, value, []string{"basic"}, []string{"github:1"}, now, false, ""},
	} {
		t.Run(name, func(t *testing.T) {
			sub, ok := readCookie(tc.key, tc.token, tc.value, tc.schemes, tc.subs, tc.at)
			if ok != tc.want || sub != tc.wantSub {
				t.Errorf("readCookie = %q, %v; want %q, %v", sub, ok, tc.wantSub, tc.want)
			}
		})
	}

	// A MAC keyed with the raw secret, the control path's token, never passes:
	// one key for two jobs would let an answer meant for one serve the other.
	payload := strings.Split(tok, ".")[1]
	m := hmac.New(sha256.New, []byte("s3cr3t"))
	m.Write([]byte("v1." + payload + "\n" + value))
	raw := "v1." + payload + "." + base64.RawURLEncoding.EncodeToString(m.Sum(nil))
	if _, ok := readCookie(key, raw, value, []string{"basic"}, nil, now); ok {
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
