// Package auth implements moose personal access tokens, caller identity
// sources, and the request-authentication middleware.
package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base32"
	"encoding/base64"
	"errors"
	"io"
	"strings"
	"time"

	"github.com/change-fabric/moose/internal/store"
)

// PATPrefix starts every token: moose_pat_<id>_<secret>.
const PATPrefix = "moose_pat_"

const (
	idLen       = 12
	secretBytes = 32
	// DefaultTokenTTL is the expiry applied when the caller does not pick one.
	DefaultTokenTTL = 90 * 24 * time.Hour
)

// ErrMalformedToken means the string is not a moose PAT.
var ErrMalformedToken = errors.New("auth: malformed token")

var idEncoding = base32.NewEncoding("abcdefghijklmnopqrstuvwxyz234567").WithPadding(base32.NoPadding)

// GeneratePAT returns a new token string and its id, reading randomness from r
// (crypto/rand.Reader when nil).
func GeneratePAT(r io.Reader) (token, id string, err error) {
	if r == nil {
		r = rand.Reader
	}
	idRaw := make([]byte, 8) // 8 bytes -> 13 base32 chars; keep 12
	if _, err := io.ReadFull(r, idRaw); err != nil {
		return "", "", err
	}
	secret := make([]byte, secretBytes)
	if _, err := io.ReadFull(r, secret); err != nil {
		return "", "", err
	}
	id = idEncoding.EncodeToString(idRaw)[:idLen]
	return PATPrefix + id + "_" + base64.RawURLEncoding.EncodeToString(secret), id, nil
}

// ParsePAT extracts the token id. The secret may itself contain '_' (base64url),
// so only the first separator after the id is significant.
func ParsePAT(token string) (id string, err error) {
	rest, ok := strings.CutPrefix(token, PATPrefix)
	if !ok {
		return "", ErrMalformedToken
	}
	id, secret, ok := strings.Cut(rest, "_")
	if !ok || len(id) != idLen || secret == "" {
		return "", ErrMalformedToken
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '2' && c <= '7') {
			return "", ErrMalformedToken
		}
	}
	if _, err := base64.RawURLEncoding.DecodeString(secret); err != nil {
		return "", ErrMalformedToken
	}
	return id, nil
}

// HashPAT is the stored form: sha256 of the full token string.
func HashPAT(token string) []byte {
	sum := sha256.Sum256([]byte(token))
	return sum[:]
}

// VerifyPAT compares token against a stored hash in constant time.
func VerifyPAT(token string, hash []byte) bool {
	return subtle.ConstantTimeCompare(HashPAT(token), hash) == 1
}

// RedactPAT renders a token safe for logs: moose_pat_<id>_***.
func RedactPAT(token string) string {
	id, err := ParsePAT(token)
	if err != nil {
		return "***"
	}
	return PATPrefix + id + "_***"
}

// NewToken mints a token for owner and returns the plaintext (shown once) and
// the record to store. ttl <= 0 means no expiry.
func NewToken(owner, name string, ttl time.Duration, now time.Time) (string, store.Token, error) {
	plain, id, err := GeneratePAT(nil)
	if err != nil {
		return "", store.Token{}, err
	}
	t := store.Token{ID: id, Owner: owner, Name: name, Hash: HashPAT(plain), Created: now.UTC()}
	if ttl > 0 {
		exp := now.UTC().Add(ttl)
		t.Expires = &exp
	}
	return plain, t, nil
}
