package credentials

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// StateProtector protects private operation evidence independently of account
// credentials. Callers bind purpose, schema, operation and account in aad.
// Open failures must never be interpreted as a missing state file.
type StateProtector interface {
	SealContext(ctx context.Context, identity string, plaintext, aad []byte) ([]byte, error)
	OpenContext(ctx context.Context, identity string, ciphertext, aad []byte) ([]byte, error)
}

type stateCiphertext struct {
	Version      int    `json:"version"`
	Type         string `json:"type"`
	ProtectedKey []byte `json:"protected_key,omitempty"`
	Nonce        []byte `json:"nonce"`
	Data         []byte `json:"data"`
}

const stateKeychainSuffix = ".recovery-state-v1"

func (s *SecureFileStore) stateService() string {
	return s.keychainServices()[0] + stateKeychainSuffix
}

func (s *SecureFileStore) stateKeyReference(identity string) string {
	return filepath.Join(s.Root, "recovery-state-keyrefs", credentialID(identity)+".json")
}

// A separate service and reference directory intentionally survive Delete.
// The durable reference also prevents silently regenerating a missing key and
// orphaning earlier recovery operations for the same identity.
func (s *SecureFileStore) stateKey(ctx context.Context, identity string, create bool) ([]byte, error) {
	account := credentialID(identity)
	read := func() ([]byte, error) {
		raw, err := s.runner()(ctx, []string{"find-generic-password", "-a", account, "-s", s.stateService(), "-w"}, "")
		if err != nil {
			return nil, err
		}
		key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(raw)))
		if err != nil || len(key) != 32 {
			return nil, ErrUnavailable
		}
		return key, nil
	}
	key, err := read()
	if errors.Is(err, os.ErrNotExist) && create {
		if _, refErr := os.Lstat(s.stateKeyReference(identity)); !errors.Is(refErr, os.ErrNotExist) {
			return nil, fmt.Errorf("recovery state key missing; existing key reference preserved: %w", ErrUnavailable)
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, errors.New("recovery state key generation failed")
		}
		encoded := base64.StdEncoding.EncodeToString(key)
		_, err = s.runner()(ctx, []string{"add-generic-password", "-a", account, "-s", s.stateService(), "-w"}, encoded+"\n"+encoded+"\n")
		if err != nil {
			// Another process may have won creation. Never overwrite its key.
			key, err = read()
		}
	}
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("recovery state key unavailable: %w", ErrUnavailable)
	}
	if create {
		digest := sha256.Sum256(key)
		ref := []byte(`{"version":1,"key_sha256":"` + hex.EncodeToString(digest[:]) + `"}` + "\n")
		existing, refErr := os.ReadFile(s.stateKeyReference(identity))
		if refErr == nil && string(existing) != string(ref) {
			return nil, fmt.Errorf("recovery state key reference mismatch: %w", ErrUnavailable)
		}
		if refErr != nil && !errors.Is(refErr, os.ErrNotExist) {
			return nil, fmt.Errorf("recovery state key reference unavailable: %w", ErrUnavailable)
		}
		// Re-flush even an existing reference: a previous attempt may have
		// replaced it but failed directory synchronization.
		if err := writePrivate(ctx, s.stateKeyReference(identity), ref); err != nil {
			return nil, err
		}
	}
	return key, nil
}

func stateAAD(identity string, aad []byte) []byte {
	bound, _ := json.Marshal(struct {
		Type     string `json:"type"`
		Identity string `json:"identity"`
		AAD      []byte `json:"aad"`
	}{"tosub2-private-state-v1", normalizedEmail(identity), aad})
	return bound
}

func stateGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func sealState(key []byte, identity string, plain, aad []byte, envelope stateCiphertext) ([]byte, error) {
	gcm, err := stateGCM(key)
	if err != nil {
		return nil, err
	}
	envelope.Version = 1
	envelope.Nonce = make([]byte, gcm.NonceSize())
	if _, err = rand.Read(envelope.Nonce); err != nil {
		return nil, err
	}
	envelope.Data = gcm.Seal(nil, envelope.Nonce, plain, stateAAD(identity, aad))
	return json.Marshal(envelope)
}

func openState(key []byte, identity string, envelope stateCiphertext, aad []byte) ([]byte, error) {
	gcm, err := stateGCM(key)
	if err != nil || len(envelope.Nonce) != 12 {
		return nil, errors.New("invalid recovery state encryption")
	}
	plain, err := gcm.Open(nil, envelope.Nonce, envelope.Data, stateAAD(identity, aad))
	if err != nil {
		return nil, errors.New("recovery state authentication failed")
	}
	return plain, nil
}

func (s *SecureFileStore) SealContext(ctx context.Context, identity string, plaintext, aad []byte) ([]byte, error) {
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	if normalizedEmail(identity) == "" {
		return nil, errors.New("recovery state identity is required")
	}
	if err := lockCredentialStore(ctx); err != nil {
		return nil, err
	}
	defer unlockCredentialStore()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var key []byte
	var err error
	envelope := stateCiphertext{}
	switch s.platform {
	case "linux":
		envelope.Type = fileKeyStateType
		key, err = s.fileKeyPurposeKey(ctx, "recovery", identity)
	case "darwin":
		envelope.Type = "keychain-recovery+aes-256-gcm"
		key, err = s.stateKey(ctx, identity, true)
	case "windows":
		envelope.Type = "dpapi-recovery+aes-256-gcm"
		key = make([]byte, 32)
		if _, err = rand.Read(key); err == nil {
			protect := s.protect
			if protect == nil {
				protect = protectDPAPI
			}
			envelope.ProtectedKey, err = protect(key)
		}
	default:
		err = ErrUnavailable
	}
	if err != nil {
		return nil, err
	}
	defer clear(key)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return sealState(key, identity, plaintext, aad, envelope)
}

func (s *SecureFileStore) OpenContext(ctx context.Context, identity string, ciphertext, aad []byte) ([]byte, error) {
	ctx, cancel := boundedContext(ctx)
	defer cancel()
	if err := lockCredentialStore(ctx); err != nil {
		return nil, err
	}
	defer unlockCredentialStore()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var envelope stateCiphertext
	if json.Unmarshal(ciphertext, &envelope) != nil || envelope.Version != 1 {
		return nil, errors.New("invalid recovery state envelope")
	}
	var key []byte
	var err error
	switch {
	case s.platform == "linux" && envelope.Type == fileKeyStateType && len(envelope.ProtectedKey) == 0:
		key, err = s.fileKeyPurposeKey(ctx, "recovery", identity)
	case s.platform == "darwin" && envelope.Type == "keychain-recovery+aes-256-gcm" && len(envelope.ProtectedKey) == 0:
		key, err = s.stateKey(ctx, identity, false)
	case s.platform == "windows" && envelope.Type == "dpapi-recovery+aes-256-gcm" && len(envelope.ProtectedKey) > 0:
		unprotect := s.unprotect
		if unprotect == nil {
			unprotect = unprotectDPAPI
		}
		key, err = unprotect(envelope.ProtectedKey)
	default:
		err = ErrUnavailable
	}
	if err != nil {
		return nil, err
	}
	defer clear(key)
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(key) != 32 {
		return nil, errors.New("invalid recovery state key")
	}
	return openState(key, identity, envelope, aad)
}

// MemoryStateProtector is an explicit ephemeral protector for tests and
// nonpersistent workflows. Its random key is lost at process exit; production
// persistent stores must use SecureFileStore instead.
type MemoryStateProtector struct{ key [32]byte }

func NewMemoryStateProtector() (*MemoryStateProtector, error) {
	p := &MemoryStateProtector{}
	_, err := rand.Read(p.key[:])
	return p, err
}

func (p *MemoryStateProtector) SealContext(ctx context.Context, identity string, plaintext, aad []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return sealState(p.key[:], identity, plaintext, aad, stateCiphertext{Type: "memory-test+aes-256-gcm"})
}

func (p *MemoryStateProtector) OpenContext(ctx context.Context, identity string, ciphertext, aad []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	var envelope stateCiphertext
	if json.Unmarshal(ciphertext, &envelope) != nil || envelope.Version != 1 || envelope.Type != "memory-test+aes-256-gcm" {
		return nil, errors.New("invalid memory recovery state envelope")
	}
	return openState(p.key[:], identity, envelope, aad)
}
