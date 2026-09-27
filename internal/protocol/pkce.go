package protocol

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
)

// PKCE contains OAuth state and the S256 verifier/challenge pair.
type PKCE struct {
	State         string `json:"state"`
	CodeVerifier  string `json:"code_verifier"`
	CodeChallenge string `json:"code_challenge"`
}

func NewPKCE() (PKCE, error) {
	v := make([]byte, 48)
	if _, e := rand.Read(v); e != nil {
		return PKCE{}, e
	}
	s := make([]byte, 24)
	if _, e := rand.Read(s); e != nil {
		return PKCE{}, e
	}
	enc := base64.RawURLEncoding
	verifier := enc.EncodeToString(v)
	h := sha256.Sum256([]byte(verifier))
	challenge := enc.EncodeToString(h[:])
	return PKCE{State: enc.EncodeToString(s), CodeVerifier: verifier, CodeChallenge: challenge}, nil
}

func VerifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	h := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(h[:]) == challenge
}
func (p PKCE) Validate() error {
	if p.State == "" || p.CodeVerifier == "" || p.CodeChallenge == "" {
		return errors.New("incomplete PKCE state")
	}
	if !VerifyPKCE(p.CodeVerifier, p.CodeChallenge) {
		return errors.New("PKCE code challenge mismatch")
	}
	return nil
}
