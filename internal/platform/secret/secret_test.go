package secret

import (
	"errors"
	"os"
	"path/filepath"
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

// Without SECRET_KEY the key is generated once into the data directory,
// readable by the owner only, and the same key is read at every start.
func TestKeyFile(t *testing.T) {
	dir := t.TempDir()
	first, created, err := KeyFile(dir)

	if err != nil || !created || len(first) != 64 {
		t.Fatalf("key %q, created %v, err %v", first, created, err)
	}

	info, err := os.Stat(filepath.Join(dir, KeyFileName))

	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("key file: %v %v", info, err)
	}

	again, created, err := KeyFile(dir)

	if err != nil || created || again != first {
		t.Fatalf("second start: key %q, created %v, err %v", again, created, err)
	}

	if _, err := New(first); err != nil {
		t.Fatal(err)
	}

	entries, _ := os.ReadDir(dir)

	if len(entries) != 1 {
		t.Errorf("left behind: %v", entries)
	}

	// A damaged file stops startup instead of replacing the key, which would
	// make the stored tokens unreadable.
	if err := os.WriteFile(filepath.Join(dir, KeyFileName), []byte("short\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, _, err := KeyFile(dir); err == nil {
		t.Error("a short key was accepted")
	}
}
