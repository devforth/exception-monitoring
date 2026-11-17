// Package scrub removes credentials from SDK payloads before they are stored.
//
// An SDK with sendDefaultPii (or a custom integration) sends request cookies
// and headers such as Authorization: the session tokens of the monitored
// app's users. Stored, they would be readable by anyone with the panel, the
// API or a copy of the database, and could be used to take over those
// sessions. So every JSON key that names a secret has its value replaced
// with Filtered before anything is parsed or stored, and so do sensitive
// query parameters in URLs and Bearer/Basic credentials in free text.
//
// The rules are key-name based (like the data scrubbing of hosted error
// trackers), so they work for every part of a payload: request headers and
// cookies, frame variables, breadcrumb data, extra, contexts and log
// attributes.
package scrub

import (
	"bytes"
	"encoding/json"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
)

// Filtered replaces a removed value.
const Filtered = "[Filtered]"

// sensitiveKey matches keys whose value is a credential. Matched anywhere in
// the key, so "Authorization", "X-Api-Key", "http.request.header.cookie" and
// "user_password" are all caught. It runs on every key of every payload, so
// ASCII keys (all of them in practice) are checked by sensitiveASCIIKey and
// remembered; scrub_test.go checks that both agree.
var sensitiveKey = regexp.MustCompile(`(?i)passw|passwd|pwd|secret|token([^s]|$)|api[-_.]?key|apikey|auth|credential|cookie|session|private[-_.]?key|csrf|xsrf|signature|dsn|card[-_.]?number|cvv|ssn`)

// credentialInText matches "Bearer <token>" and "Basic <token>" in strings.
var credentialInText = regexp.MustCompile(`(?i)\b(bearer|basic)\s+[A-Za-z0-9._~+/=-]{8,}`)

// knownKeys remembers keys already checked: payloads repeat the same few
// hundred. Bounded, so random keys cannot grow it without end.
var (
	knownKeys     sync.Map
	knownKeyCount atomic.Int64
)

const maxKnownKeys = 10000

// IsSensitiveKey reports whether a value stored under key is a credential.
func IsSensitiveKey(key string) bool {
	if known, ok := knownKeys.Load(key); ok {
		return known.(bool)
	}

	var sensitive bool

	if isASCII(key) {
		sensitive = sensitiveASCIIKey(strings.ToLower(key))
	} else {
		sensitive = sensitiveKey.MatchString(key) // Unicode case folding, as before
	}

	if knownKeyCount.Load() < maxKnownKeys {
		if _, loaded := knownKeys.LoadOrStore(key, sensitive); !loaded {
			knownKeyCount.Add(1)
		}
	}

	return sensitive
}

// sensitiveWords are the parts of sensitiveKey without separators or
// conditions ("passwd" is covered by "passw").
var sensitiveWords = []string{"passw", "pwd", "secret", "apikey", "auth", "credential", "cookie", "session", "csrf", "xsrf", "signature", "dsn", "cvv", "ssn"}

// sensitiveASCIIKey is sensitiveKey for a lowercase ASCII key.
func sensitiveASCIIKey(key string) bool {
	for _, word := range sensitiveWords {
		if strings.Contains(key, word) {
			return true
		}
	}

	// "token" not followed by "s": "tokens" alone is a count, "tokenizer" is not.
	for rest := key; ; {
		index := strings.Index(rest, "token")

		if index < 0 {
			break
		}

		after := index + len("token")

		if after == len(rest) || rest[after] != 's' {
			return true
		}

		rest = rest[after:]
	}

	// api[-_.]?key, private[-_.]?key, card[-_.]?number
	for _, pair := range [...][2]string{{"api", "key"}, {"private", "key"}, {"card", "number"}} {
		for rest := key; ; {
			index := strings.Index(rest, pair[0])

			if index < 0 {
				break
			}

			after := rest[index+len(pair[0]):]

			if strings.HasPrefix(after, pair[1]) || len(after) > 0 && strings.IndexByte("-_.", after[0]) >= 0 && strings.HasPrefix(after[1:], pair[1]) {
				return true
			}

			rest = rest[index+1:]
		}
	}

	return false
}

func isASCII(text string) bool {
	for index := 0; index < len(text); index++ {
		if text[index] >= 0x80 {
			return false
		}
	}

	return true
}

// containsFoldASCII reports whether text contains word (lowercase ASCII) in any case.
func containsFoldASCII(text, word string) bool {
	for index := 0; index+len(word) <= len(text); index++ {
		if text[index]|0x20 == word[0] && strings.EqualFold(text[index:index+len(word)], word) {
			return true
		}
	}

	return false
}

// JSON returns payload with credentials replaced. Input that is not valid
// JSON is returned unchanged: the parser rejects it later anyway.
func JSON(payload []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber() // keep numbers exactly as sent

	var document any

	if err := decoder.Decode(&document); err != nil {
		return payload
	}

	var output bytes.Buffer

	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(Value(document)); err != nil {
		return payload
	}

	return bytes.TrimRight(output.Bytes(), "\n")
}

// Value removes credentials from an already decoded JSON value in place.
// Numbers should be decoded with json.Decoder.UseNumber to preserve precision.
func Value(node any) any {
	switch typed := node.(type) {
	case map[string]any:
		for key, child := range typed {
			if IsSensitiveKey(key) {
				typed[key] = filtered(child)
			} else {
				typed[key] = Value(child)
			}
		}

		return typed
	case []any:
		// Headers and query strings may come as [name, value] pairs.
		if len(typed) == 2 {
			if name, ok := typed[0].(string); ok && IsSensitiveKey(name) {
				typed[1] = filtered(typed[1])

				return typed
			}
		}

		for index, child := range typed {
			typed[index] = Value(child)
		}

		return typed
	case string:
		return Text(typed)
	default:
		return node
	}
}

// filtered replaces a sensitive value. A log attribute keeps its
// {"value", "type"} shape, so the log still parses.
func filtered(node any) any {
	if attribute, ok := node.(map[string]any); ok {
		if _, hasValue := attribute["value"]; hasValue {
			attribute["value"] = Filtered
			attribute["type"] = "string"

			return attribute
		}
	}

	if node == nil {
		return nil
	}

	return Filtered
}

// Text removes credentials from free text: Bearer/Basic tokens and
// sensitive query parameters of URLs ("?token=…") or query strings.
func Text(text string) string {
	if text == "" {
		return text
	}

	// The regexps run only when their words are there: most strings have none
	// (non-ASCII text always: the regexp also folds letters such as ſ to s).
	if !isASCII(text) || containsFoldASCII(text, "bearer") || containsFoldASCII(text, "basic") {
		text = credentialInText.ReplaceAllString(text, "$1 "+Filtered)
	}

	if !strings.Contains(text, "=") {
		return text
	}

	// A whole URL, or a bare query string such as request.query_string.
	if parsed, err := url.Parse(text); err == nil && parsed.RawQuery != "" && !strings.ContainsAny(text, " \t\n") {
		return withQuery(parsed, text)
	}

	if !strings.ContainsAny(text, " \t\n/:") && strings.Contains(text, "=") {
		return query(text)
	}

	if !strings.Contains(text, "://") {
		return text
	}

	return queryInText.ReplaceAllStringFunc(text, func(match string) string {
		parsed, err := url.Parse(match)

		if err != nil || parsed.RawQuery == "" {
			return match
		}

		return withQuery(parsed, match)
	})
}

// withQuery returns the URL with its query scrubbed, or original untouched
// when nothing in it is sensitive (re-encoding could change it otherwise).
func withQuery(parsed *url.URL, original string) string {
	scrubbed := query(parsed.RawQuery)

	if scrubbed == parsed.RawQuery {
		return original
	}

	parsed.RawQuery = scrubbed

	return parsed.String()
}

// queryInText finds URLs with a query string inside longer text.
var queryInText = regexp.MustCompile(`(?i)\b[a-z][a-z0-9+.-]*://[^\s"'<>]+\?[^\s"'<>]+`)

func query(raw string) string {
	values, err := url.ParseQuery(raw)

	if err != nil {
		return raw
	}

	changed := false

	for key := range values {
		if IsSensitiveKey(key) {
			values[key] = []string{Filtered}
			changed = true
		}
	}

	if !changed {
		return raw
	}

	return values.Encode()
}
