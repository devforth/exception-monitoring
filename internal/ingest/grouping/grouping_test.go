package grouping

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/fixtures"
	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
)

func fixtureEvent(t *testing.T, name string) *sentry.Event {
	t.Helper()

	envelope, err := sentry.ParseEnvelope(fixtures.Envelope(t, name))

	if err != nil {
		t.Fatal(err)
	}

	event, err := sentry.ParseEvent(envelope.Items[0].Payload, envelope.Header.EventID, time.Now())

	if err != nil {
		t.Fatal(err)
	}

	return event
}

func parse(t *testing.T, payload string) *sentry.Event {
	t.Helper()

	event, err := sentry.ParseEvent([]byte(payload), "", time.Now())

	if err != nil {
		t.Fatal(err)
	}

	return event
}

// exceptionEvent builds an event with one exception and in-app frames given as "file|function|line".
func exceptionEvent(t *testing.T, errorType, value string, frames ...string) *sentry.Event {
	t.Helper()

	var frameJSON []string

	for _, frame := range frames {
		parts := strings.Split(frame, "|")
		frameJSON = append(frameJSON, `{"filename":"`+parts[0]+`","function":"`+parts[1]+`","lineno":`+parts[2]+`,"in_app":true}`)
	}

	return parse(t, `{"exception":{"values":[{"type":"`+errorType+`","value":"`+value+`",
		"stacktrace":{"frames":[{"filename":"node_modules/lib.js","function":"lib","in_app":false},`+strings.Join(frameJSON, ",")+`]}}]}}`)
}

func TestSameProblemGroupsTogether(t *testing.T) {
	testCases := map[string][2]*sentry.Event{
		"different value text": {
			exceptionEvent(t, "Error", "Order 890 not found", "app.js|load|10"),
			exceptionEvent(t, "Error", "Order 891 not found", "app.js|load|10"),
		},
		"code moved after a deploy": {
			exceptionEvent(t, "Error", "boom", "app.js|load|10", "app.js|main|3"),
			exceptionEvent(t, "Error", "boom", "app.js|load|42", "app.js|main|7"),
		},
		"message with ids": {
			parse(t, `{"message":"User 42 failed login from 10.0.0.1, request 9ec79c33-ec99-42ab-8353-589fcb2e04dc"}`),
			parse(t, `{"message":"User 7 failed login from 10.0.0.2, request 1b2c3d4e-0000-4000-8000-000000000001"}`),
		},
		"browser origin and bundle hash": {
			exceptionEvent(t, "TypeError", "x", "http://localhost:3500/assets/main.3f2a1b9c.js?v=1|render|1"),
			exceptionEvent(t, "TypeError", "x", "https://app.example.com/assets/main.77aa00ff.js|render|1"),
		},
		// tsx watch adds "?<timestamp>" on every restart; the Node SDK copies it into the module.
		"cache-busting query in module": {
			parse(t, `{"exception":{"values":[{"type":"Error","value":"x","stacktrace":{"frames":[
				{"module":"tsx.dist.esm:index.mjs?1794411765314","filename":"/app/index.mjs?1794411765314","function":"F","in_app":false}]}}]}}`),
			parse(t, `{"exception":{"values":[{"type":"Error","value":"x","stacktrace":{"frames":[
				{"module":"tsx.dist.esm:index.mjs?1794411844942","filename":"/app/index.mjs?1794411844942","function":"F","in_app":false}]}}]}}`),
		},
	}

	for name, events := range testCases {
		t.Run(name, func(t *testing.T) {
			if Fingerprint(events[0]) != Fingerprint(events[1]) {
				t.Fatalf("want one group:\n%q\n%q", Components(events[0]), Components(events[1]))
			}
		})
	}
}

func TestDifferentProblemsSplit(t *testing.T) {
	testCases := map[string][2]*sentry.Event{
		"different type": {
			exceptionEvent(t, "TypeError", "boom", "app.js|load|10"),
			exceptionEvent(t, "RangeError", "boom", "app.js|load|10"),
		},
		"different place": {
			exceptionEvent(t, "Error", "boom", "app.js|load|10"),
			exceptionEvent(t, "Error", "boom", "app.js|save|10"),
		},
		"different message": {
			parse(t, `{"message":"Payment provider responded slowly"}`),
			parse(t, `{"message":"Payment provider is down"}`),
		},
		"no stack, different value": {
			parse(t, `{"exception":{"values":[{"type":"Error","value":"disk full"}]}}`),
			parse(t, `{"exception":{"values":[{"type":"Error","value":"permission denied"}]}}`),
		},
		"same message, different level of the chain": {
			parse(t, `{"exception":{"values":[{"type":"SyntaxError","value":"x"},{"type":"Error","value":"load failed"}]}}`),
			parse(t, `{"exception":{"values":[{"type":"TimeoutError","value":"x"},{"type":"Error","value":"load failed"}]}}`),
		},
	}

	for name, events := range testCases {
		t.Run(name, func(t *testing.T) {
			if Fingerprint(events[0]) == Fingerprint(events[1]) {
				t.Fatalf("want two groups, both are %q", Components(events[0]))
			}
		})
	}
}

// sourceEvent is an Error thrown in function at lineno, with the source line the SDK sends.
func sourceEvent(t *testing.T, function string, lineno int, source string) *sentry.Event {
	t.Helper()

	return parse(t, fmt.Sprintf(`{"exception":{"values":[{"type":"Error","value":"x","stacktrace":{"frames":[
		{"filename":"routes.ts","function":%q,"lineno":%d,"in_app":true,"context_line":%q}]}}]}}`, function, lineno, source))
}

func TestSourceLineSeparatesSameNamedFunctions(t *testing.T) {
	// JS names `handler: () => {...}` after the property, so many functions in
	// one file are called "handler". Found in the playground: 9 different
	// errors merged into one issue.
	checkout := sourceEvent(t, "handler", 120, `throw new Error("Checkout failed");`)
	invoice := sourceEvent(t, "handler", 240, `throw new Error("Could not generate invoice PDF");`)

	if Fingerprint(checkout) == Fingerprint(invoice) {
		t.Fatal("different throws in functions with the same name must not merge")
	}

	// The same line after a deploy moved it: still the same issue.
	moved := sourceEvent(t, "handler", 135, `    throw new Error("Checkout failed");`)

	if Fingerprint(checkout) != Fingerprint(moved) {
		t.Fatalf("a moved line must stay in its group:\n%q\n%q", Components(checkout), Components(moved))
	}
}

func TestAnonymousFunctions(t *testing.T) {
	handler := func(line string) *sentry.Event {
		return parse(t, `{"exception":{"values":[{"type":"Error","value":"x","stacktrace":{"frames":[
			{"filename":"routes.ts","function":"<anonymous>","lineno":10,"in_app":true,"context_line":"`+line+`"}]}}]}}`)
	}
	// Two different Express route handlers in one file: both are "<anonymous>".
	if Fingerprint(handler("throw new Error('no orders')")) == Fingerprint(handler("throw new Error('no users')")) {
		t.Fatal("different anonymous functions must not merge")
	}
	// Indentation changes don't matter.
	if Fingerprint(handler("  throw new Error('no orders')")) != Fingerprint(handler("throw new Error('no orders')")) {
		t.Fatal("whitespace must not split a group")
	}
}

func TestStackWithoutSourceAddsValue(t *testing.T) {
	// Browser SDKs send no source lines, so a shared frame is weak evidence.
	if Fingerprint(fixtureEvent(t, "browser-event-handled")) == Fingerprint(fixtureEvent(t, "browser-event-unhandled-rejection")) {
		t.Fatal("different browser errors in one handler must not merge")
	}
	// With source lines the value is not used: the stack is enough.
	withSource := func(value string) *sentry.Event {
		return parse(t, `{"exception":{"values":[{"type":"Error","value":"`+value+`","stacktrace":{"frames":[
			{"filename":"a.ts","function":"load","in_app":true,"context_line":"throw err"}]}}]}}`)
	}

	if Fingerprint(withSource("user alice not found")) != Fingerprint(withSource("user bob not found")) {
		t.Fatal("with source lines, the value must not split groups")
	}
}

func TestCustomFingerprint(t *testing.T) {
	base := `"exception":{"values":[{"type":"Error","value":"boom","stacktrace":{"frames":[{"filename":"a.js","function":"f","in_app":true}]}}]}`
	custom := parse(t, `{"fingerprint":["payment","stripe"],`+base+`}`)
	sameCustomOtherStack := parse(t, `{"fingerprint":["payment","stripe"],"message":"other"}`)
	withDefault := parse(t, `{"fingerprint":["{{ default }}","eu"],`+base+`}`)
	plain := parse(t, `{`+base+`}`)

	if Fingerprint(custom) != Fingerprint(sameCustomOtherStack) {
		t.Fatal("a custom fingerprint must override the stack")
	}

	if got := Components(withDefault); got[len(got)-1] != "eu" || !strings.Contains(strings.Join(got, " "), "frame:a.js f") {
		t.Fatalf("{{ default }} not expanded: %q", got)
	}

	if Fingerprint(withDefault) == Fingerprint(plain) {
		t.Fatal("{{ default }} plus an extra value must differ from the default alone")
	}
	// A custom value must not collide with a default component of the same text.
	if Fingerprint(parse(t, `{"fingerprint":["message","hi"]}`)) == Fingerprint(parse(t, `{"message":"hi"}`)) {
		t.Fatal("custom and default fingerprints collide")
	}
}

func TestFixtures(t *testing.T) {
	message := fixtureEvent(t, "node-event-message")

	if got := Components(message); got[0] != "message" {
		t.Fatalf("captureMessage must group by message, got %q", got)
	}

	unhandled := fixtureEvent(t, "node-event-unhandled")
	components := strings.Join(Components(unhandled), "\n")

	if strings.Contains(components, "node_modules") {
		t.Fatalf("library frames must be skipped when there are in-app frames:\n%s", components)
	}

	if Fingerprint(fixtureEvent(t, "node-event-custom-fingerprint")) == Fingerprint(unhandled) {
		t.Fatal("custom fingerprint fixture grouped with an unrelated error")
	}

	if Culprit(unhandled) == "" || Culprit(fixtureEvent(t, "browser-event-onerror")) == "" {
		t.Fatal("empty culprit")
	}
}

func TestNormalizeMessage(t *testing.T) {
	testCases := map[string]string{
		"Order 890 not found":                          "Order <n> not found",
		"Sentry test error 2026-11-11T13:31:10.278Z":   "Sentry test error <n>-<n>-<n>T<n>:<n>:<n>.<n>Z",
		"request 9ec79c33-ec99-42ab-8353-589fcb2e04dc": "request <uuid>",
		"object 0x7f3a9c00ffee12 freed":                "object <hex> freed",
		"commit deadbeefcafe1234":                      "commit <hex>",
		"short hex abc12 stays text-ish":               "short hex abc<n> stays text-ish",
		"Cannot read properties of undefined":          "Cannot read properties of undefined",
	}

	for input, want := range testCases {
		if got := NormalizeMessage(input); got != want {
			t.Errorf("NormalizeMessage(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestCulprit(t *testing.T) {
	if got := Culprit(parse(t, `{"transaction":"GET /api/orders","message":"x"}`)); got != "GET /api/orders" {
		t.Fatalf("culprit = %q", got)
	}

	event := exceptionEvent(t, "Error", "x", "src/orders.ts|loadOrders|10", "src/db.ts|query|5")

	if got := Culprit(event); got != "query (src/db.ts)" {
		t.Fatalf("culprit = %q, want the frame that threw", got)
	}
}

func TestLogFingerprint(t *testing.T) {
	same := LogFingerprint("order 17 failed for 0b7c9a3e-1d2f-4a5b-8c6d-7e8f9a0b1c2d")

	if same != LogFingerprint("order 42 failed for 9a8b7c6d-5e4f-4a3b-8c2d-1e0f9a8b7c6d") {
		t.Error("the same message with other values must share a fingerprint")
	}

	if same == LogFingerprint("payment failed") {
		t.Error("different messages must not share a fingerprint")
	}
}
