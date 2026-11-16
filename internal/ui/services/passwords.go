package services

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// PasswordIterations follows the OWASP recommendation for PBKDF2-SHA256. A
// variable only so tests can lower it (internal/ui/export_test.go): the race
// detector makes 600k iterations take seconds. The count is stored with
// each hash, so raising it later keeps old passwords working.
var PasswordIterations = 600_000

const passwordScheme = "pbkdf2-sha256"

// HashPassword returns the stored form of a password:
// pbkdf2-sha256$<iterations>$<salt>$<key>, a random salt per password.
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)

	if _, err := rand.Read(salt); err != nil {
		return "", err
	}

	key, err := pbkdf2.Key(sha256.New, password, salt, PasswordIterations, 32)

	if err != nil {
		return "", fmt.Errorf("hash password: %w", err)
	}

	encode := base64.RawStdEncoding.EncodeToString

	return strings.Join([]string{passwordScheme, strconv.Itoa(PasswordIterations), encode(salt), encode(key)}, "$"), nil
}

// checkPassword compares a password with a stored hash in constant time.
func checkPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")

	if len(parts) != 4 || parts[0] != passwordScheme {
		return false
	}

	iterations, err := strconv.Atoi(parts[1])
	salt, saltErr := base64.RawStdEncoding.DecodeString(parts[2])
	want, keyErr := base64.RawStdEncoding.DecodeString(parts[3])

	if err != nil || saltErr != nil || keyErr != nil || iterations < 1 || len(want) == 0 {
		return false
	}

	key, err := pbkdf2.Key(sha256.New, password, salt, iterations, len(want))

	return err == nil && subtle.ConstantTimeCompare(key, want) == 1
}

// dummyPasswordHash is checked for unknown usernames, so a login takes as
// long whether the user exists or not.
var dummyPasswordHash = sync.OnceValue(func() string {
	hash, _ := HashPassword("unknown user")

	return hash
})

// temporaryPassword is a random password someone else sets for a user, who
// must replace it at the next login (rand.Text: 26 characters, 130 bits).
func temporaryPassword() string {
	return rand.Text()
}
