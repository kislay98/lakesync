// Package secrets seals and opens secrets at rest. The local implementation uses
// AES-256-GCM with a key from the environment; a cloud KMS can be plugged in as
// another SecretStore without touching the core.
package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// SecretStore seals plaintext for storage and opens it again.
type SecretStore interface {
	Seal(plaintext []byte) ([]byte, error)
	Open(ciphertext []byte) ([]byte, error)
}

// Version byte of the ciphertext layout: v1 = AES-256-GCM, 12-byte nonce.
const v1 byte = 1

type aesgcm struct {
	aead cipher.AEAD
}

// NewAESGCM builds a store from a 32-byte key.
func NewAESGCM(key []byte) (SecretStore, error) {
	if len(key) != 32 {
		return nil, fmt.Errorf("secrets: key must be 32 bytes, got %d", len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &aesgcm{aead: aead}, nil
}

// ParseKey accepts a 32-byte key as 64 hex characters, standard/URL base64, or raw.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil, errors.New("APP_SECRET_KEY is not set")
	}
	if len(s) == 64 {
		if b, err := hex.DecodeString(s); err == nil {
			return b, nil
		}
	}
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == 32 {
			return b, nil
		}
	}
	if len(s) == 32 {
		return []byte(s), nil
	}
	return nil, errors.New("APP_SECRET_KEY must be 32 bytes, given as 64 hex characters, base64, or a 32-character string")
}

// GenerateKey returns a fresh random key as hex.
func GenerateKey() string {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func (a *aesgcm) Seal(plaintext []byte) ([]byte, error) {
	nonce := make([]byte, a.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(nonce)+len(plaintext)+a.aead.Overhead())
	out = append(out, v1)
	out = append(out, nonce...)
	return a.aead.Seal(out, nonce, plaintext, []byte{v1}), nil
}

func (a *aesgcm) Open(ciphertext []byte) ([]byte, error) {
	ns := a.aead.NonceSize()
	if len(ciphertext) < 1+ns+a.aead.Overhead() {
		return nil, errors.New("secrets: ciphertext too short")
	}
	if ciphertext[0] != v1 {
		return nil, fmt.Errorf("secrets: unknown ciphertext version %d", ciphertext[0])
	}
	nonce := ciphertext[1 : 1+ns]
	pt, err := a.aead.Open(nil, nonce, ciphertext[1+ns:], []byte{v1})
	if err != nil {
		return nil, errors.New("secrets: cannot open ciphertext (wrong APP_SECRET_KEY?)")
	}
	return pt, nil
}
