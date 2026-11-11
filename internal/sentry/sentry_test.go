package sentry

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Real SDK captures from docs/sentry-protocol.md.
const fixturesDir = "../../testdata/envelopes"

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(fixturesDir, name+".envelope"))
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func parseFixture(t *testing.T, name string) *Envelope {
	t.Helper()
	envelope, err := ParseEnvelope(readFixture(t, name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return envelope
}

func firstEvent(t *testing.T, name string) *Event {
	t.Helper()
	envelope := parseFixture(t, name)
	if len(envelope.Items) != 1 || envelope.Items[0].Header.Type != ItemEvent {
		t.Fatalf("%s: want one event item, got %+v", name, envelope.Items)
	}
	event, err := ParseEvent(envelope.Items[0].Payload, envelope.Header.EventID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestAllFixturesParse(t *testing.T) {
	files, err := filepath.Glob(filepath.Join(fixturesDir, "*.envelope"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures found: %v", err)
	}
	for _, file := range files {
		name := strings.TrimSuffix(filepath.Base(file), ".envelope")
		t.Run(name, func(t *testing.T) {
			envelope := parseFixture(t, name)
			if len(envelope.Items) == 0 {
				t.Fatal("no items")
			}
			for _, item := range envelope.Items {
				switch item.Header.Type {
				case ItemEvent:
					if _, err := ParseEvent(item.Payload, envelope.Header.EventID, time.Now()); err != nil {
						t.Fatal(err)
					}
				case ItemLog:
					if _, err := ParseLogs(item.Payload, time.Now()); err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestNodeUnhandledEvent(t *testing.T) {
	event := firstEvent(t, "node-event-unhandled")
	if len(event.EventID) != 32 {
		t.Fatalf("event_id = %q", event.EventID)
	}
	if event.Platform != "node" || event.Level != "error" {
		t.Fatalf("platform=%q level=%q", event.Platform, event.Level)
	}
	main := event.MainException()
	if main == nil || main.Mechanism == nil || main.Mechanism.Handled == nil || *main.Mechanism.Handled {
		t.Fatalf("want an unhandled exception, got %+v", main)
	}
	if main.Stacktrace == nil || len(main.Stacktrace.Frames) == 0 {
		t.Fatal("no stack frames")
	}
	if event.Timestamp.Year() < 2026 {
		t.Fatalf("timestamp = %v", event.Timestamp)
	}
	if event.TraceID() == "" {
		t.Fatal("no trace_id in contexts")
	}
}

func TestCauseChainTitleUsesOutermostError(t *testing.T) {
	event := firstEvent(t, "node-event-cause-chain")
	if len(event.Exception) < 2 {
		t.Fatalf("want a cause chain, got %d exceptions", len(event.Exception))
	}
	if event.Title() == event.Exception[0].Type+": "+event.Exception[0].Value {
		t.Fatalf("title %q is the innermost cause, want the outermost error", event.Title())
	}
}

func TestMessageEvent(t *testing.T) {
	event := firstEvent(t, "node-event-message")
	if event.Message != "Payment provider responded slowly" || event.Level != "warning" {
		t.Fatalf("message=%q level=%q", event.Message, event.Level)
	}
	// captureMessage adds an exception without a type: the title is the message text.
	if event.Title() != "Payment provider responded slowly" {
		t.Fatalf("title = %q", event.Title())
	}
}

func TestHandledEventContext(t *testing.T) {
	event := firstEvent(t, "node-event-handled-context")
	if event.Tags["feature"] != "checkout" {
		t.Fatalf("tags = %v", event.Tags)
	}
	if event.User == nil || event.User.ID != "42" || event.User.Email != "john@example.com" {
		t.Fatalf("user = %+v", event.User)
	}
	if len(event.Breadcrumbs) == 0 {
		t.Fatal("no breadcrumbs")
	}
}

func TestCustomFingerprint(t *testing.T) {
	event := firstEvent(t, "node-event-custom-fingerprint")
	if len(event.Fingerprint) == 0 {
		t.Fatal("fingerprint not parsed")
	}
}

func TestBrowserEvent(t *testing.T) {
	event := firstEvent(t, "browser-event-onerror")
	if event.Platform != "javascript" {
		t.Fatalf("platform = %q", event.Platform)
	}
	if !strings.HasPrefix(event.Title(), "ReferenceError: ") {
		t.Fatalf("title = %q", event.Title())
	}
}

func TestLogs(t *testing.T) {
	envelope := parseFixture(t, "node-logs")
	logs, err := ParseLogs(envelope.Items[0].Payload, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != envelope.Items[0].Header.ItemCount {
		t.Fatalf("got %d logs, header says %d", len(logs), envelope.Items[0].Header.ItemCount)
	}
	for _, record := range logs {
		if record.Body == "" || record.Level == "" || record.Timestamp.IsZero() {
			t.Fatalf("incomplete log: %+v", record)
		}
	}
	origins := map[string]bool{}
	for _, record := range logs {
		origins[record.Attributes["sentry.origin"].String()] = true
	}
	if !origins["auto.log.pino"] {
		t.Fatalf("no pino logs among origins %v", origins)
	}
}

func TestEnvelopeWithLengthAndTrailingNewline(t *testing.T) {
	// The first payload contains a newline, so only "length" can delimit it.
	body := "{\"event_id\":\"9ec79c33ec9942ab8353589fcb2e04dc\"}\n" +
		"{\"type\":\"attachment\",\"length\":7}\nab\ncdef\n" +
		"{\"type\":\"event\"}\n{\"message\":\"hi\"}\n"
	envelope, err := ParseEnvelope([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(envelope.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(envelope.Items))
	}
	if string(envelope.Items[0].Payload) != "ab\ncdef" {
		t.Fatalf("attachment payload = %q", envelope.Items[0].Payload)
	}
	event, err := ParseEvent(envelope.Items[1].Payload, envelope.Header.EventID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if event.EventID != "9ec79c33ec9942ab8353589fcb2e04dc" || event.Message != "hi" {
		t.Fatalf("event_id=%q message=%q", event.EventID, event.Message)
	}
}

func TestEnvelopeErrors(t *testing.T) {
	testCases := map[string]string{
		"empty":             "",
		"bad header":        "not json\n",
		"bad item header":   "{}\nnot json\n{}",
		"length too large":  "{}\n{\"type\":\"event\",\"length\":100}\n{}",
		"negative length":   "{}\n{\"type\":\"event\",\"length\":-1}\n{}",
		"empty item header": "{}\n\n{\"type\":\"event\"}\n{}",
		"too many items":    "{}\n" + strings.Repeat("{\"type\":\"event\"}\n{}\n", MaxItems+1),
	}
	for name, body := range testCases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseEnvelope([]byte(body)); err == nil {
				t.Fatal("expected an error")
			}
		})
	}
}

func TestLenientFieldShapes(t *testing.T) {
	// Shapes allowed by the protocol but not sent by the JS SDK.
	payload := `{
		"event_id": "9EC79C33-EC99-42AB-8353-589FCB2E04DC",
		"timestamp": "2026-11-10T16:27:00.5Z",
		"logentry": {"formatted": "formatted text", "message": "template %s"},
		"exception": [{"type": "ValueError", "value": "bad"}],
		"breadcrumbs": {"values": [{"message": "step"}]},
		"tags": [["release_stage", "beta"], ["attempt", 3]],
		"fingerprint": ["{{ default }}", 42],
		"user": {"id": 7},
		"level": "fatal"
	}`
	event, err := ParseEvent([]byte(payload), "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	checks := map[string][2]string{
		"event_id":    {event.EventID, "9ec79c33ec9942ab8353589fcb2e04dc"},
		"timestamp":   {event.Timestamp.Format(time.RFC3339Nano), "2026-11-10T16:27:00.5Z"},
		"message":     {string(event.Message), "formatted text"},
		"title":       {event.Title(), "ValueError: bad"},
		"breadcrumb":  {event.Breadcrumbs[0].Message, "step"},
		"tag string":  {event.Tags["release_stage"], "beta"},
		"tag number":  {event.Tags["attempt"], "3"},
		"fingerprint": {strings.Join(event.Fingerprint, ","), "{{ default }},42"},
		"user id":     {string(event.User.ID), "7"},
	}
	for name, check := range checks {
		if check[0] != check[1] {
			t.Errorf("%s = %q, want %q", name, check[0], check[1])
		}
	}
}

func TestEventDefaults(t *testing.T) {
	received := time.Date(2026, 11, 11, 10, 0, 0, 0, time.UTC)
	// A wrong-shaped field (tags as a number) is dropped, not fatal.
	event, err := ParseEvent([]byte(`{"tags": 5}`), "not-an-id", received)
	if err != nil {
		t.Fatal(err)
	}
	if len(event.EventID) != 32 || !event.Timestamp.Equal(received) || event.Level != "error" {
		t.Fatalf("event_id=%q timestamp=%v level=%q", event.EventID, event.Timestamp, event.Level)
	}
	if event.Title() != "<unlabeled event>" {
		t.Fatalf("title = %q", event.Title())
	}
}

func TestInvalidEventJSON(t *testing.T) {
	if _, err := ParseEvent([]byte(`{"message": `), "", time.Now()); err == nil {
		t.Fatal("expected an error")
	}
}
