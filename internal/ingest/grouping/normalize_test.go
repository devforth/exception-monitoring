package grouping

import (
	"math/rand"
	"regexp"
	"testing"
)

// The regexps NormalizeMessage replaced: its output must stay the same, or
// the same errors would group into new issues.
var (
	referenceUUID   = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	referenceHex    = regexp.MustCompile(`\b(0x)?[0-9a-fA-F]{8,}\b`)
	referenceNumber = regexp.MustCompile(`\d+`)
)

func referenceNormalize(message string) string {
	message = referenceUUID.ReplaceAllString(message, "<uuid>")
	message = referenceHex.ReplaceAllString(message, "<hex>")

	return referenceNumber.ReplaceAllString(message, "<n>")
}

func TestNormalizeMessageMatchesTheRegexps(t *testing.T) {
	for _, message := range []string{
		"", "order 17 failed", "user 550e8400-e29b-41d4-a716-446655440000 not found", "id 0xDEADBEEF01 at 0x7ffd",
		"hash deadbeefcafe1234, short abc123", "deadbeefz", "_deadbeef", "x0xdeadbeef", "0xdeadbee", "0x0xdeadbeef00",
		"550e8400-e29b-41d4-a716-4466554400001", "ab-550e8400-e29b-41d4-a716-446655440000-cd", "привіт 12345678 світ",
		"line 42:7 deadbeef_", "ffffffff.ffffffff", "0xffffffffg", "1e10 3.14 -7", "x" + "0123456789abcdef0123456789abcdef",
	} {
		if got, want := NormalizeMessage(message), referenceNormalize(message); got != want {
			t.Errorf("NormalizeMessage(%q) = %q, want %q", message, got, want)
		}
	}

	// Random text from the characters the patterns care about.
	alphabet := []byte("0123456789abcdefABCDEFxXg_- .:/\n" + "é")
	random := rand.New(rand.NewSource(1))

	for range 200000 {
		message := make([]byte, random.Intn(60))

		for index := range message {
			message[index] = alphabet[random.Intn(len(alphabet))]
		}

		if got, want := NormalizeMessage(string(message)), referenceNormalize(string(message)); got != want {
			t.Fatalf("NormalizeMessage(%q) = %q, want %q", message, got, want)
		}
	}
}

func BenchmarkNormalizeMessage(b *testing.B) {
	message := "GET /api/orders/12345 failed for user 550e8400-e29b-41d4-a716-446655440000 after 3 retries (trace deadbeefcafe1234)"

	b.Run("written out", func(b *testing.B) {
		for b.Loop() {
			NormalizeMessage(message)
		}
	})
	b.Run("regexps", func(b *testing.B) {
		for b.Loop() {
			referenceNormalize(message)
		}
	})
}
