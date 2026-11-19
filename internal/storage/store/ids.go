package store

import (
	"crypto/rand"
	"math/big"
)

// IDs of users, agents, notification destinations and connections, and issues
// are IDLength random characters of idAlphabet: one cannot be guessed from
// another. The store makes them and checks they are free.
const (
	IDLength   = 8
	idAlphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
)

// NewID returns a random ID.
func NewID() string {
	id := make([]byte, IDLength)
	size := big.NewInt(int64(len(idAlphabet)))

	for index := range id {
		number, err := rand.Int(rand.Reader, size)

		if err != nil {
			panic("crypto/rand failed: " + err.Error())
		}

		id[index] = idAlphabet[number.Int64()]
	}

	return string(id)
}

// ValidID reports whether id has the shape of an ID, so a request with
// anything else is refused before a lookup.
func ValidID(id string) bool {
	if len(id) != IDLength {
		return false
	}

	for index := range len(id) {
		if ('a' > id[index] || id[index] > 'z') && ('0' > id[index] || id[index] > '9') {
			return false
		}
	}

	return true
}
