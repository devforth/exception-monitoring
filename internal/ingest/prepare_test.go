package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/devforth/exception-monitoring/internal/fixtures"
	"github.com/devforth/exception-monitoring/internal/ingest/compact"
	"github.com/devforth/exception-monitoring/internal/ingest/scrub"
	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
)

// The one pass gives exactly what two passes give: scrub.JSON, then
// compact.EventFields on the result decoded again.
func TestPrepareEventMatchesSeparatePasses(t *testing.T) {
	payloads := [][]byte{
		[]byte(`{"message":"Bearer syntheticcredential","request":{"headers":[["Authorization","syntheticsecret"]]},"extra":{"n":12345678901234567890,"password":"syntheticsecret"},"modules":{"unused":"1"},"exception":{"values":[{"stacktrace":{"frames":[{"in_app":true,"vars":{"password":"syntheticsecret"}},{"in_app":false,"vars":{"large":"unused"}}]}}]}}`),
		[]byte(`{"breadcrumbs":{"values":[` + strings.Repeat(`{"message":"step"},`, 79) + `{"message":"last"}]}}`),
		[]byte("not json"), []byte("null"), []byte(`["Bearer syntheticcredential"]`),
	}

	for _, fixture := range fixtures.All(t) {
		envelope, err := sentry.ParseEnvelope(fixture.Body)

		if err != nil {
			t.Fatal(err)
		}

		for _, item := range envelope.Items {
			if item.Header.Type == sentry.ItemEvent {
				payloads = append(payloads, item.Payload)
			}
		}
	}

	for index, payload := range payloads {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			want := twoPasses(payload)
			got := prepareEvent(payload)

			if !bytes.Equal(got, want) {
				t.Fatalf("combined output differs from existing behavior:\ngot %s\nwant %s", got, want)
			}
		})
	}
}

var prepared []byte

func BenchmarkPrepareEvent(b *testing.B) {
	envelope, err := sentry.ParseEnvelope(fixtures.Envelope(b, "node-event-handled-context"))

	if err != nil {
		b.Fatal(err)
	}

	var payload []byte

	for _, item := range envelope.Items {
		if item.Header.Type == sentry.ItemEvent {
			payload = item.Payload
			break
		}
	}

	if len(payload) == 0 {
		b.Fatal("no event")
	}
	// Also exercise a larger event with many fields, not only one large string.
	var large map[string]any

	if err := json.Unmarshal(payload, &large); err != nil {
		b.Fatal(err)
	}

	extra := map[string]any{}

	for index := range 200 {
		extra[fmt.Sprint(index)] = map[string]any{"message": strings.Repeat("synthetic diagnostic context ", 8), "password": "syntheticsecret", "number": 12345}
	}

	large["extra"] = extra
	largePayload, err := json.Marshal(large)

	if err != nil {
		b.Fatal(err)
	}

	for _, sample := range []struct {
		name string
		data []byte
	}{{"sdk", payload}, {"large", largePayload}} {
		for _, method := range []struct {
			name string
			run  func([]byte) []byte
		}{{"separate", twoPasses}, {"combined", prepareEvent}} {
			b.Run(sample.name+"/"+method.name, func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(sample.data)))

				for b.Loop() {
					prepared = method.run(sample.data)
				}
			})
		}
	}
}

// twoPasses is the preparation as it was before prepareEvent: scrub.JSON,
// then the compaction on the result decoded again.
func twoPasses(payload []byte) []byte {
	scrubbed := scrub.JSON(payload)
	decoder := json.NewDecoder(bytes.NewReader(scrubbed))
	decoder.UseNumber()

	var event map[string]any

	if err := decoder.Decode(&event); err != nil || event == nil {
		return scrubbed
	}

	compact.EventFields(event)

	var output bytes.Buffer

	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(event); err != nil {
		return scrubbed
	}

	return bytes.TrimRight(output.Bytes(), "\n")
}
