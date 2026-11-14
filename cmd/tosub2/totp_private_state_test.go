package main

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"testing"

	"tosub2-go-port/internal/privatestate"
)

// The deterministic key is deliberately test-only and protects exclusively
// temporary synthetic fixtures. It lets the abrupt-exit helper and its parent
// read the same encrypted journal without accessing any real OS keychain.
type runnerSyntheticStateProtector struct{ key [32]byte }

func registerRunnerSyntheticProtection(t *testing.T, root string) {
	t.Helper()
	p := &runnerSyntheticStateProtector{key: sha256.Sum256([]byte("synthetic-runner-test-only:" + root))}
	if err := privatestate.Register(root, p); err != nil {
		t.Fatal(err)
	}
}

func (p *runnerSyntheticStateProtector) aead() (cipher.AEAD, error) {
	block, err := aes.NewCipher(p.key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

func (p *runnerSyntheticStateProtector) SealContext(ctx context.Context, identity string, plaintext, aad []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aead, err := p.aead()
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return aead.Seal(nonce, nonce, plaintext, append([]byte(identity+"\x00"), aad...)), nil
}

func (p *runnerSyntheticStateProtector) OpenContext(ctx context.Context, identity string, ciphertext, aad []byte) ([]byte, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	aead, err := p.aead()
	if err != nil {
		return nil, err
	}
	if len(ciphertext) < aead.NonceSize() {
		return nil, errors.New("invalid synthetic ciphertext")
	}
	return aead.Open(nil, ciphertext[:aead.NonceSize()], ciphertext[aead.NonceSize():], append([]byte(identity+"\x00"), aad...))
}
