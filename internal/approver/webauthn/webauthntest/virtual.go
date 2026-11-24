// Package webauthntest is a software authenticator for tests: it answers
// WebAuthn registration and assertion options the way a platform passkey does
// (ES256, no attestation), with switches to leave the UV flag off.
package webauthntest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"

	"github.com/fxamacker/cbor/v2"
	"github.com/go-webauthn/webauthn/protocol"
)

// Authenticator is one virtual passkey.
type Authenticator struct {
	RPID   string
	Origin string
	// UserVerified sets the UV flag on what the authenticator produces.
	UserVerified bool

	key     *ecdsa.PrivateKey
	credID  []byte
	counter uint32
}

// New returns an authenticator for the relying party with UV on.
func New(rpID, origin string) *Authenticator {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	id := make([]byte, 32)
	_, _ = rand.Read(id)
	return &Authenticator{RPID: rpID, Origin: origin, UserVerified: true, key: k, credID: id}
}

func b64(b []byte) string { return base64.RawURLEncoding.EncodeToString(b) }

const (
	flagUP = 0x01
	flagUV = 0x04
	flagAT = 0x40
)

func (a *Authenticator) flags(extra byte) byte {
	f := byte(flagUP) | extra
	if a.UserVerified {
		f |= flagUV
	}
	return f
}

func (a *Authenticator) rpHash() []byte {
	h := sha256.Sum256([]byte(a.RPID))
	return h[:]
}

func clientData(typ string, challenge []byte, origin string) []byte {
	b, _ := json.Marshal(map[string]any{"type": typ, "challenge": b64(challenge), "origin": origin, "crossOrigin": false})
	return b
}

// Register answers creation options with an attestation response body.
func (a *Authenticator) Register(creation *protocol.CredentialCreation) []byte {
	cd := clientData("webauthn.create", creation.Response.Challenge, a.Origin)
	x, y := a.key.PublicKey.X.FillBytes(make([]byte, 32)), a.key.PublicKey.Y.FillBytes(make([]byte, 32))
	cose, err := cbor.Marshal(map[int]any{1: 2, 3: -7, -1: 1, -2: x, -3: y})
	if err != nil {
		panic(err)
	}
	auth := append([]byte{}, a.rpHash()...)
	auth = append(auth, a.flags(flagAT))
	auth = binary.BigEndian.AppendUint32(auth, a.counter)
	auth = append(auth, make([]byte, 16)...) // aaguid
	auth = binary.BigEndian.AppendUint16(auth, uint16(len(a.credID)))
	auth = append(auth, a.credID...)
	auth = append(auth, cose...)
	att, err := cbor.Marshal(map[string]any{"fmt": "none", "attStmt": map[string]any{}, "authData": auth})
	if err != nil {
		panic(err)
	}
	body, _ := json.Marshal(map[string]any{
		"id": b64(a.credID), "rawId": b64(a.credID), "type": "public-key",
		"response": map[string]any{"clientDataJSON": b64(cd), "attestationObject": b64(att)},
	})
	return body
}

// Assert answers assertion options with an assertion response body.
func (a *Authenticator) Assert(req *protocol.CredentialAssertion) []byte {
	return a.AssertChallenge(req.Response.Challenge)
}

// AssertChallenge signs an assertion over an explicit challenge, for tests that
// present a challenge issued elsewhere.
func (a *Authenticator) AssertChallenge(challenge []byte) []byte {
	a.counter++
	cd := clientData("webauthn.get", challenge, a.Origin)
	auth := append([]byte{}, a.rpHash()...)
	auth = append(auth, a.flags(0))
	auth = binary.BigEndian.AppendUint32(auth, a.counter)
	cdh := sha256.Sum256(cd)
	digest := sha256.Sum256(append(append([]byte{}, auth...), cdh[:]...))
	sig, err := ecdsa.SignASN1(rand.Reader, a.key, digest[:])
	if err != nil {
		panic(err)
	}
	body, _ := json.Marshal(map[string]any{
		"id": b64(a.credID), "rawId": b64(a.credID), "type": "public-key",
		"response": map[string]any{
			"clientDataJSON": b64(cd), "authenticatorData": b64(auth), "signature": b64(sig),
		},
	})
	return body
}
