// Package secret encrypts values stored in the database (chat tokens, webhook
// URLs) with a key from the environment, so a copy of the database file alone
// does not reveal them. The key must be kept apart from the data directory.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
)

// MinKeyLength keeps the key out of guessing range; any longer string works.
const MinKeyLength = 32

// Box encrypts and decrypts with AES-256-GCM. The zero value is unusable;
// create it with New.
type Box struct {
	aead cipher.AEAD
}

// New derives a 256-bit key from the configured string (SHA-256), so any
// sufficiently long random string can be used, e.g. `openssl rand -hex 32`.
func New(key string) (*Box, error) {
	if len(key) < MinKeyLength {
		return nil, fmt.Errorf("secret key must be at least %d characters", MinKeyLength)
	}

	sum := sha256.Sum256([]byte(key))

	block, err := aes.NewCipher(sum[:])

	if err != nil {
		return nil, err
	}

	aead, err := cipher.NewGCM(block)

	if err != nil {
		return nil, err
	}

	return &Box{aead: aead}, nil
}

// Encrypt returns base64(nonce || ciphertext). Every call uses a new random
// nonce, so equal values encrypt differently.
func (box *Box) Encrypt(plaintext []byte) (string, error) {
	nonce := make([]byte, box.aead.NonceSize())

	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}

	sealed := box.aead.Seal(nonce, nonce, plaintext, nil)

	return base64.StdEncoding.EncodeToString(sealed), nil
}

// ErrDecrypt means the value was encrypted with another key or was changed.
var ErrDecrypt = errors.New("cannot decrypt: wrong SECRET_KEY or damaged value")

func (box *Box) Decrypt(encoded string) ([]byte, error) {
	sealed, err := base64.StdEncoding.DecodeString(encoded)

	if err != nil || len(sealed) < box.aead.NonceSize() {
		return nil, ErrDecrypt
	}

	nonce, ciphertext := sealed[:box.aead.NonceSize()], sealed[box.aead.NonceSize():]

	plaintext, err := box.aead.Open(nil, nonce, ciphertext, nil)

	if err != nil {
		return nil, ErrDecrypt
	}

	return plaintext, nil
}
