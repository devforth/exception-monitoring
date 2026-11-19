// Package secret encrypts values stored in the database (chat tokens, webhook
// URLs), so a copy of the database (DATA_DIR/leveldb) alone does not reveal
// them. The key comes from SECRET_KEY, kept apart from the data; without it
// one is generated into the data directory (KeyFile), which protects a leaked
// copy of the database but not a copy of the whole data directory.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
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

// KeyFileName is the generated key in the data directory.
const KeyFileName = "secret.key"

// KeyFile returns the key stored in dir, creating it (32 random bytes as
// hex, readable by the owner only) when there is none; created says so. The
// file appears whole or not at all, so a crash or a second process starting
// at the same moment never leaves a short or different key.
func KeyFile(dir string) (key string, created bool, err error) {
	path := filepath.Join(dir, KeyFileName)

	if key, err := readKey(path); !errors.Is(err, fs.ErrNotExist) {
		return key, false, err
	}

	random := make([]byte, 32)

	if _, err := rand.Read(random); err != nil {
		return "", false, err
	}

	temporary, err := os.CreateTemp(dir, KeyFileName+".*")

	if err != nil {
		return "", false, err
	}

	// The temporary name goes in any case: after the link the key file is
	// another name for the same data.
	defer func() { _ = os.Remove(temporary.Name()) }()

	_, err = temporary.WriteString(hex.EncodeToString(random) + "\n")

	if err == nil {
		err = temporary.Sync()
	}

	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}

	if err != nil {
		return "", false, err
	}

	// Link fails if the file exists: another process made it first.
	err = os.Link(temporary.Name(), path)

	if errors.Is(err, fs.ErrExist) {
		key, err := readKey(path)

		return key, false, err
	}

	if err != nil {
		return "", false, err
	}

	return hex.EncodeToString(random), true, nil
}

func readKey(path string) (string, error) {
	content, err := os.ReadFile(path)

	if err != nil {
		return "", err
	}

	key := strings.TrimSpace(string(content))

	if len(key) < MinKeyLength {
		return "", fmt.Errorf("%s: the key must be at least %d characters", path, MinKeyLength)
	}

	return key, nil
}
