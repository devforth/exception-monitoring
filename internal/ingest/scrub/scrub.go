// Package scrub removes credentials from SDK payloads before they are parsed
// or stored: values of keys that name a secret, Bearer/Basic credentials and
// sensitive query parameters become Filtered.
package scrub

import (
	"bytes"
	"encoding/json"
	"encoding/json/jsontext"
	"io"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"

	textutil "github.com/devforth/exception-monitoring/internal/platform/text"
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

// Only keys this short are remembered: real keys are short, and long ones
// sent on purpose must not fill memory (10000 keys of 64 bytes at most).
const (
	maxKnownKeys      = 10000
	maxKnownKeyLength = 64
)

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

	if len(key) <= maxKnownKeyLength && knownKeyCount.Load() < maxKnownKeys {
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

// NewDecoder reads a payload as a token stream. SDKs may repeat a key or send
// invalid UTF-8; both are accepted, as the event parser accepts them.
func NewDecoder(payload []byte) *jsontext.Decoder {
	return jsontext.NewDecoder(bytes.NewReader(payload), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
}

// NewEncoder writes a token stream compactly, without HTML escaping.
func NewEncoder(output io.Writer) *jsontext.Encoder {
	return jsontext.NewEncoder(output, jsontext.EscapeForHTML(false), jsontext.AllowDuplicateNames(true), jsontext.AllowInvalidUTF8(true))
}

// JSON returns payload with credentials replaced, read and written as one
// token stream (no tree is built). Input that is not valid JSON is returned
// unchanged: the parser rejects it later anyway.
func JSON(payload []byte) []byte {
	var output bytes.Buffer

	if err := Stream(NewDecoder(payload), NewEncoder(&output)); err != nil {
		return payload
	}

	return bytes.TrimRight(output.Bytes(), "\n")
}

// Stream copies the next value from decoder to encoder with credentials
// removed: values of keys that name a secret, the second of a [name, value]
// pair whose name does, and credentials in strings (Text).
func Stream(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
	switch decoder.PeekKind() {
	case '{':
		return Object(decoder, encoder, nil)
	case '[':
		return array(decoder, encoder)
	case '"':
		return stringValue(decoder, encoder)
	}

	value, err := decoder.ReadValue()

	if err != nil {
		return err
	}

	return encoder.WriteValue(value)
}

// Object copies the next object member by member. member, when set, may take
// one over (write it, or drop it by writing nothing) and says whether it did;
// a member whose key names a secret is filtered before member sees it.
func Object(decoder *jsontext.Decoder, encoder *jsontext.Encoder,
	member func(name string, decoder *jsontext.Decoder, encoder *jsontext.Encoder) (bool, error),
) error {
	if err := copyToken(decoder, encoder); err != nil {
		return err
	}

	for decoder.PeekKind() != '}' {
		key, err := decoder.ReadToken()

		if err != nil {
			return err
		}

		name := key.String()

		if IsSensitiveKey(name) {
			if err := encoder.WriteToken(jsontext.String(name)); err != nil {
				return err
			}

			if err := writeFiltered(decoder, encoder); err != nil {
				return err
			}

			continue
		}

		if member != nil {
			if done, err := member(name, decoder, encoder); err != nil || done {
				if err != nil {
					return err
				}

				continue
			}
		}

		if err := encoder.WriteToken(jsontext.String(name)); err != nil {
			return err
		}

		if err := Stream(decoder, encoder); err != nil {
			return err
		}
	}

	return copyToken(decoder, encoder)
}

// Values copies the next array element by element through each.
func Values(decoder *jsontext.Decoder, encoder *jsontext.Encoder, each func(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error) error {
	if err := copyToken(decoder, encoder); err != nil {
		return err
	}

	for decoder.PeekKind() != ']' {
		if err := each(decoder, encoder); err != nil {
			return err
		}
	}

	return copyToken(decoder, encoder)
}

// array copies an array. One led by a secret's name may be a [name, value]
// pair (headers and query strings may come this way), whose value is
// filtered; that takes its length, so such an array (a small one in
// practice) is read whole and scrubbed as a tree: rereading each element
// as a stream would take time growing with the square of its nesting.
func array(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
	if err := copyToken(decoder, encoder); err != nil {
		return err
	}

	if decoder.PeekKind() == '"' {
		first, err := decoder.ReadValue()

		if err != nil {
			return err
		}

		first = first.Clone() // a read value is valid only until the next read

		var name string

		if err := json.Unmarshal(first, &name); err != nil {
			return err
		}

		if !IsSensitiveKey(name) {
			if err := writeText(encoder, first, name); err != nil {
				return err
			}
		} else {
			return ledBySecret(decoder, encoder, first, name)
		}
	}

	for decoder.PeekKind() != ']' {
		if err := Stream(decoder, encoder); err != nil {
			return err
		}
	}

	return copyToken(decoder, encoder)
}

// ledBySecret writes the rest of an array whose first element, a secret's
// name, is read: a pair's value is filtered, the elements of a longer array
// are scrubbed as trees.
func ledBySecret(decoder *jsontext.Decoder, encoder *jsontext.Encoder, first jsontext.Value, name string) error {
	var rest []jsontext.Value

	for decoder.PeekKind() != ']' {
		element, err := decoder.ReadValue()

		if err != nil {
			return err
		}

		rest = append(rest, element.Clone())
	}

	if _, err := decoder.ReadToken(); err != nil {
		return err
	}

	if len(rest) == 1 {
		if err := encoder.WriteValue(first); err != nil {
			return err
		}

		if err := writeFiltered(NewDecoder(rest[0]), encoder); err != nil {
			return err
		}
	} else {
		if err := writeText(encoder, first, name); err != nil {
			return err
		}

		for _, element := range rest {
			reader := json.NewDecoder(bytes.NewReader(element))
			reader.UseNumber()

			var node any

			if err := reader.Decode(&node); err != nil {
				return err
			}

			if err := writeJSON(encoder, tree(node)); err != nil {
				return err
			}
		}
	}

	return encoder.WriteToken(jsontext.EndArray)
}

// tree removes credentials from a decoded value in place, by the rules of Stream.
func tree(node any) any {
	switch typed := node.(type) {
	case map[string]any:
		for key, child := range typed {
			if IsSensitiveKey(key) {
				typed[key] = filtered(child)
			} else {
				typed[key] = tree(child)
			}
		}

		return typed
	case []any:
		if len(typed) == 2 {
			if name, ok := typed[0].(string); ok && IsSensitiveKey(name) {
				typed[1] = filtered(typed[1])

				return typed
			}
		}

		for index, child := range typed {
			typed[index] = tree(child)
		}

		return typed
	case string:
		return Text(typed)
	default:
		return node
	}
}

// filtered is what replaces a secret's value in a tree (see writeFiltered).
func filtered(node any) any {
	if attribute, ok := node.(map[string]any); ok {
		if _, hasValue := attribute["value"]; hasValue {
			attribute["value"], attribute["type"] = Filtered, "string"

			return attribute
		}
	}

	if node == nil {
		return nil
	}

	return Filtered
}

func copyToken(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
	token, err := decoder.ReadToken()

	if err != nil {
		return err
	}

	return encoder.WriteToken(token)
}

// stringValue copies a string, through Text only when it may hold a
// credential; most strings are copied as they came.
func stringValue(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
	raw, err := decoder.ReadValue()

	if err != nil {
		return err
	}

	if !mayHoldCredential(raw) {
		return encoder.WriteValue(raw)
	}

	var text string

	if err := json.Unmarshal(raw, &text); err != nil {
		return err
	}

	return writeText(encoder, raw, text)
}

// writeText writes text through Text; unchanged, the raw value as it came.
func writeText(encoder *jsontext.Encoder, raw jsontext.Value, text string) error {
	if scrubbed := Text(text); scrubbed != text {
		return encoder.WriteToken(jsontext.String(scrubbed))
	}

	return encoder.WriteValue(raw)
}

// mayHoldCredential reports whether Text could change a raw JSON string: it
// has "=" (a query), "bearer" or "basic" in any case, or an escape or a byte
// beyond ASCII, which are checked the slow way.
func mayHoldCredential(raw []byte) bool {
	for index, char := range raw {
		switch {
		case char == '=' || char == '\\' || char >= 0x80:
			return true
		case char|0x20 == 'b':
			rest := raw[index:]

			if len(rest) >= 5 && bytes.EqualFold(rest[:5], []byte("basic")) || len(rest) >= 6 && bytes.EqualFold(rest[:6], []byte("bearer")) {
				return true
			}
		}
	}

	return false
}

// writeFiltered replaces the next value: null stays null, a log attribute
// keeps its {"value", "type"} shape so the log still parses, anything else
// becomes Filtered.
func writeFiltered(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
	raw, err := decoder.ReadValue()

	if err != nil {
		return err
	}

	switch raw.Kind() {
	case 'n':
		return encoder.WriteValue(raw)
	case '{':
		attribute := map[string]any{}
		reader := json.NewDecoder(bytes.NewReader(raw))
		reader.UseNumber()

		if reader.Decode(&attribute) == nil {
			if _, hasValue := attribute["value"]; hasValue {
				return writeJSON(encoder, filtered(attribute))
			}
		}
	}

	return encoder.WriteToken(jsontext.String(Filtered))
}

func writeJSON(encoder *jsontext.Encoder, value any) error {
	var output bytes.Buffer

	writer := json.NewEncoder(&output)
	writer.SetEscapeHTML(false)

	if err := writer.Encode(value); err != nil {
		return err
	}

	return encoder.WriteValue(bytes.TrimRight(output.Bytes(), "\n"))
}

// Text removes credentials from free text: Bearer/Basic tokens and
// sensitive query parameters of URLs ("?token=…") or query strings.
func Text(text string) string {
	if text == "" {
		return text
	}

	// The regexps run only when their words are there: most strings have none
	// (non-ASCII text always: the regexp also folds letters such as ſ to s).
	if !isASCII(text) || textutil.ContainsFold(text, "bearer") || textutil.ContainsFold(text, "basic") {
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
