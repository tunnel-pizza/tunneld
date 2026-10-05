package auth

import (
	"crypto/pbkdf2"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"strconv"
	"strings"

	"golang.org/x/text/unicode/norm"
)

// Iteration bounds: tunnel.pizza writes 600,000. The ceiling keeps a value
// from making every login burn a CPU; the floor keeps one from being cheap
// to guess against.
const (
	minIter = 100_000
	maxIter = 2_000_000
)

// phc is a parsed $pbkdf2-sha256$i=<iter>$<salt>$<hash>, salt and hash in
// unpadded standard base64.
type phc struct {
	iter       int
	salt, hash []byte
}

func parsePHC(s string) (phc, error) {
	parts := strings.Split(s, "$")
	if len(parts) != 5 || parts[0] != "" || parts[1] != "pbkdf2-sha256" || !strings.HasPrefix(parts[2], "i=") {
		return phc{}, errors.New("pw is not a $pbkdf2-sha256$i=…$…$… string")
	}
	iter, err := strconv.Atoi(strings.TrimPrefix(parts[2], "i="))
	if err != nil || iter < minIter || iter > maxIter {
		return phc{}, errors.New("pw: iterations must be a number from 100000 to 2000000")
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil || len(salt) < 16 {
		return phc{}, errors.New("pw: the salt must be at least 16 bytes of unpadded base64")
	}
	hash, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil || len(hash) != 32 {
		return phc{}, errors.New("pw: the hash must be 32 bytes of unpadded base64")
	}
	return phc{iter: iter, salt: salt, hash: hash}, nil
}

// verify reports whether password hashes to p: NFC-normalized (RFC 8265's
// OpaqueString profile, which the browser applies before hashing too), as
// UTF-8, compared in constant time. An empty password never verifies: the
// browser that set it could not check a length nobody downstream ever sees.
func (p phc) verify(password string) bool {
	if password == "" {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, norm.NFC.String(password), p.salt, p.iter, len(p.hash))
	return err == nil && subtle.ConstantTimeCompare(got, p.hash) == 1
}
