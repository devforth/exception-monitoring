package secret

import (
	"errors"
	"strings"
	"testing"
)

const key = "0123456789abcdef0123456789abcdef"

func TestRoundTrip(t *testing.T) {
	box, err := New(key)

	if err != nil {
		t.Fatal(err)
	}

	encrypted, err := box.Encrypt([]byte("https://hooks.slack.com/services/T0/B0/secret"))

	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(encrypted, "hooks.slack.com") {
		t.Fatal("the stored value must not contain the plaintext")
	}

	again, _ := box.Encrypt([]byte("https://hooks.slack.com/services/T0/B0/secret"))

	if again == encrypted {
		t.Fatal("equal values must encrypt differently (random nonce)")
	}

	decrypted, err := box.Decrypt(encrypted)

	if err != nil || string(decrypted) != "https://hooks.slack.com/services/T0/B0/secret" {
		t.Fatalf("decrypted %q, %v", decrypted, err)
	}
}

func TestWrongKeyOrDamagedValue(t *testing.T) {
	box, _ := New(key)
	other, _ := New(strings.Repeat("x", 40))

	encrypted, _ := box.Encrypt([]byte("token"))

	if _, err := other.Decrypt(encrypted); !errors.Is(err, ErrDecrypt) {
		t.Fatalf("other key: %v", err)
	}

	damaged := []byte(encrypted)
	damaged[len(damaged)/2] ^= 1

	for _, value := range []string{string(damaged), "not base64!", ""} {
		if _, err := box.Decrypt(value); !errors.Is(err, ErrDecrypt) {
			t.Fatalf("%q: %v", value, err)
		}
	}
}

func TestShortKeyRejected(t *testing.T) {
	if _, err := New("short"); err == nil {
		t.Fatal("expected an error for a short key")
	}
}
