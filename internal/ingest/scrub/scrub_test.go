package scrub

import (
	"encoding/json"
	"math/rand"
	"strings"
	"testing"
)

func TestJSONRemovesCredentials(t *testing.T) {
	payload := `{
	  "event_id": "0123456789abcdef0123456789abcdef",
	  "message": "login failed for Bearer eyJhbGciOiJIUzI1NiJ9.payload.signature",
	  "request": {
	    "url": "https://app.example.com/orders?id=7&access_token=query-token-value",
	    "query_string": "id=7&api_key=query-key-value",
	    "headers": {"Authorization": "Bearer header-token-value", "Cookie": "sid=cookie-value", "X-Api-Key": "api-key-value", "User-Agent": "curl/8"},
	    "cookies": {"sid": "cookie-object-value"},
	    "data": {"email": "user@example.com", "password": "body-password-value"}
	  },
	  "exception": {"values": [{"type": "Error", "value": "boom", "stacktrace": {"frames": [
	    {"function": "login", "lineno": 3, "vars": {"sessionToken": "frame-var-value", "count": 12345678901234567890}}
	  ]}}]},
	  "breadcrumbs": {"values": [{"category": "http", "data": {"url": "https://api.example.com/v1?token=breadcrumb-token-value", "method": "GET"}}]},
	  "extra": {"headers": [["authorization", "pair-token-value"], ["accept", "*/*"]]},
	  "contexts": {"trace": {"trace_id": "4bf92f3577b34da6a3ce929d0e0e4736", "span_id": "00f067aa0ba902b7"}}
	}`

	scrubbed := string(JSON([]byte(payload)))

	for _, secret := range []string{
		"query-token-value", "query-key-value", "header-token-value", "cookie-value", "cookie-object-value",
		"api-key-value", "body-password-value", "frame-var-value", "breadcrumb-token-value", "pair-token-value", "eyJhbGci",
	} {
		if strings.Contains(scrubbed, secret) {
			t.Errorf("%q survived:\n%s", secret, scrubbed)
		}
	}

	// What is needed to debug stays, numbers exactly as sent.
	for _, kept := range []string{"curl/8", "user@example.com", "id=7", `"method":"GET"`, "4bf92f3577b34da6a3ce929d0e0e4736", "12345678901234567890", `"function":"login"`, "*/*"} {
		if !strings.Contains(scrubbed, kept) {
			t.Errorf("%q was removed:\n%s", kept, scrubbed)
		}
	}

	if !json.Valid([]byte(scrubbed)) {
		t.Fatal("scrubbed payload is not valid JSON")
	}
}

func TestJSONKeepsLogAttributeShape(t *testing.T) {
	payload := `{"items": [{"body": "request", "attributes": {"http.request.header.cookie": {"value": "sid=secret", "type": "string"}, "gen_ai.usage.input_tokens": {"value": 42, "type": "integer"}}}]}`

	scrubbed := string(JSON([]byte(payload)))

	if strings.Contains(scrubbed, "sid=secret") || !strings.Contains(scrubbed, `"http.request.header.cookie":{"type":"string","value":"[Filtered]"}`) {
		t.Errorf("cookie attribute:\n%s", scrubbed)
	}

	// Counts of tokens are not tokens.
	if !strings.Contains(scrubbed, `"value":42`) {
		t.Errorf("token count was removed:\n%s", scrubbed)
	}
}

func TestJSONLeavesInvalidInput(t *testing.T) {
	if got := string(JSON([]byte("not json"))); got != "not json" {
		t.Fatalf("got %q", got)
	}
}

func TestText(t *testing.T) {
	testCases := map[string]string{
		"plain error text": "plain error text",
		"x = a == b":       "x = a == b",
		"/orders?page=2":   "/orders?page=2",
		"call https://x.example.com/a?token=abc done": "call https://x.example.com/a?token=%5BFiltered%5D done",
		"Authorization: Basic dXNlcjpwYXNzd29yZA==":   "Authorization: Basic [Filtered]",
	}

	for input, want := range testCases {
		if got := Text(input); got != want {
			t.Errorf("Text(%q) = %q, want %q", input, got, want)
		}
	}
}

// IsSensitiveKey checks ASCII keys without the regexp: it must agree with it.
func TestIsSensitiveKeyMatchesTheRegexp(t *testing.T) {
	keys := []string{
		"Authorization", "X-Api-Key", "api_key", "api.key", "api-key", "apikey", "API_KEY", "apiKey", "api__key", "privatekey", "private-key",
		"card_number", "card number", "cardNumber", "tokens", "token", "Tokens", "access_token", "tokens_token", "tokenizer", "max_tokens",
		"passwd", "user_password", "pwd", "secret", "session_id", "csrf", "xsrf_token", "signature", "dsn", "cvv", "ssn", "cookie",
		"credentials", "url", "headers", "user_id", "release", "", "a", "authority", "tokenstoken", "toke", "apikeyz", "privatekeyring",
	}
	alphabet := []byte("apikeytoknsrwdcuhvbmPIKETOS_-. ")
	random := rand.New(rand.NewSource(1))

	for range 200000 {
		key := make([]byte, random.Intn(14))

		for index := range key {
			key[index] = alphabet[random.Intn(len(alphabet))]
		}

		keys = append(keys, string(key))
	}

	for _, key := range keys {
		if got, want := sensitiveASCIIKey(strings.ToLower(key)), sensitiveKey.MatchString(key); got != want {
			t.Fatalf("key %q: %v, the regexp says %v", key, got, want)
		}

		if IsSensitiveKey(key) != sensitiveKey.MatchString(key) {
			t.Fatalf("IsSensitiveKey(%q) disagrees", key)
		}
	}
}
