package ingest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/fixtures"
	"github.com/devforth/exception-monitoring/internal/ingest/scrub"
	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
)

// The token pass gives what the tree pass it replaced gave (reference*
// below, kept as it was): the same JSON once decoded. Key order and string
// escapes may differ; the stored payload is always decoded again.
func TestPrepareMatchesTheTreePass(t *testing.T) {
	for index, payload := range samplePayloads(t) {
		t.Run(fmt.Sprint(index), func(t *testing.T) {
			if got, want := normalized(prepareEvent(payload)), normalized(referencePrepare(payload)); got != want {
				t.Fatalf("event of %s:\ngot  %s\nwant %s", payload, got, want)
			}

			if got, want := normalized(scrub.JSON(payload)), normalized(referenceScrub(payload)); got != want {
				t.Fatalf("scrub of %s:\ngot  %s\nwant %s", payload, got, want)
			}
		})
	}
}

func FuzzPrepareMatchesTheTreePass(f *testing.F) {
	for _, payload := range samplePayloads(f) {
		f.Add(payload)
	}

	f.Fuzz(func(t *testing.T, payload []byte) {
		if got, want := normalized(prepareEvent(payload)), normalized(referencePrepare(payload)); got != want {
			t.Fatalf("event of %q:\ngot  %s\nwant %s", payload, got, want)
		}

		if got, want := normalized(scrub.JSON(payload)), normalized(referenceScrub(payload)); got != want {
			t.Fatalf("scrub of %q:\ngot  %s\nwant %s", payload, got, want)
		}
	})
}

// samplePayloads are the events and logs of every fixture plus the edge cases of the rules.
func samplePayloads(t testing.TB) [][]byte {
	payloads := [][]byte{
		[]byte(`{"message":"Bearer syntheticcredential","request":{"headers":[["Authorization","syntheticsecret"]]},"extra":{"n":12345678901234567890,"password":"syntheticsecret"},"modules":{"unused":"1"},"exception":{"values":[{"stacktrace":{"frames":[{"in_app":true,"vars":{"password":"syntheticsecret"}},{"in_app":false,"vars":{"large":"unused"}}]}}]}}`),
		[]byte(`{"breadcrumbs":{"values":[` + strings.Repeat(`{"message":"step"},`, 79) + `{"message":"last"}]}}`),
		[]byte(`{"breadcrumbs":[` + strings.Repeat(`{"message":"step"},`, 60) + `{"message":"last"}],"threads":[{"stacktrace":{"frames":[{"in_app":true},{"filename":"x.js","vars":{"a":1}}]}}]}`),
		[]byte(`{"breadcrumbs":null,"sdk":{"name":"node","integrations":["a"],"packages":[{"name":"b"}],"token":"x"},"debug_meta":{"images":[]},"stacktrace":{"frames":[{"in_app":true,"filename":"/app/node_modules/x.js","context_line":"lib"},{"in_app":true,"filename":"/app/a.js","context_line":"app"}]}}`),
		[]byte(`{"a":"x\u0062earer abcdefghijkl","b":"\u00e9 Bearer abcdefghijkl","c":"https://h.example/p?\u0074oken=1","d":"plain \/ <b>&","e":"\xff\xfe broken utf8","f":"\ud800 lone"}`),
		[]byte(`{"pairs":[["Cookie","sid=1"],["cookie","a","b"],["accept","*/*"],["x"],[]],"attr":{"session.id":{"value":"s","type":"string"},"api_key":null,"secret":{"nested":1},"token":[1,2]}}`),
		[]byte(`{"dup":"Bearer abcdefghijkl","dup":"second","password":"a","password":"b"} trailing`),
		[]byte(`{"exception":{"values":"not a list"},"threads":{"values":[1,"x",null]},"stacktrace":{"frames":"none"}}`),
		[]byte(`{"items":[{"body":"request","attributes":{"http.request.header.authorization":{"value":"Bearer abcdefghijkl","type":"string"},"gen_ai.usage.input_tokens":{"value":42,"type":"integer"}}}]}`),
		[]byte("not json"), []byte("null"), []byte(""), []byte(`["Bearer syntheticcredential"]`), []byte(`"just a string ?token=1"`), []byte("12.50"),
		[]byte(`{"a":`), []byte(`{"a":1,}`), []byte(`[1,2`),
	}

	for _, fixture := range fixtures.All(t) {
		envelope, err := sentry.ParseEnvelope(fixture.Body)

		if err != nil {
			t.Fatal(err)
		}

		for _, item := range envelope.Items {
			if item.Header.Type == sentry.ItemEvent || item.Header.Type == sentry.ItemLog {
				payloads = append(payloads, item.Payload)
			}
		}
	}

	return payloads
}

// normalized is valid JSON decoded and written again with sorted keys, so
// outputs that differ only in key order or escapes compare equal; anything
// else is compared as it is.
func normalized(payload []byte) string {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()

	var value any

	if decoder.Decode(&value) != nil {
		return "raw " + string(payload)
	}

	encoded, _ := json.Marshal(value)

	return string(encoded)
}

// referencePrepare is the tree pass prepareEvent replaced, kept as it was:
// decode everything, remove credentials, then drop what does not help a fix.
func referencePrepare(payload []byte) []byte {
	return referencePass(payload, func(document any) any {
		document = referenceValue(document)

		if event, ok := document.(map[string]any); ok && event != nil {
			referenceEventFields(event)
		}

		return document
	})
}

// referenceScrub is the tree pass scrub.JSON replaced.
func referenceScrub(payload []byte) []byte {
	return referencePass(payload, referenceValue)
}

func referencePass(payload []byte, change func(any) any) []byte {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()

	var document any

	if err := decoder.Decode(&document); err != nil {
		return payload
	}

	var output bytes.Buffer

	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(change(document)); err != nil {
		return payload
	}

	return bytes.TrimRight(output.Bytes(), "\n")
}

func referenceValue(node any) any {
	switch typed := node.(type) {
	case map[string]any:
		for key, child := range typed {
			if scrub.IsSensitiveKey(key) {
				typed[key] = referenceFiltered(child)
			} else {
				typed[key] = referenceValue(child)
			}
		}

		return typed
	case []any:
		if len(typed) == 2 {
			if name, ok := typed[0].(string); ok && scrub.IsSensitiveKey(name) {
				typed[1] = referenceFiltered(typed[1])

				return typed
			}
		}

		for index, child := range typed {
			typed[index] = referenceValue(child)
		}

		return typed
	case string:
		return scrub.Text(typed)
	default:
		return node
	}
}

func referenceFiltered(node any) any {
	if attribute, ok := node.(map[string]any); ok {
		if _, hasValue := attribute["value"]; hasValue {
			attribute["value"] = scrub.Filtered
			attribute["type"] = "string"

			return attribute
		}
	}

	if node == nil {
		return nil
	}

	return scrub.Filtered
}

func referenceEventFields(event map[string]any) {
	delete(event, "modules")
	delete(event, "debug_meta")

	if sdk, ok := event["sdk"].(map[string]any); ok {
		delete(sdk, "integrations")
		delete(sdk, "packages")
	}

	event["breadcrumbs"] = referenceNewestBreadcrumbs(event["breadcrumbs"])

	if event["breadcrumbs"] == nil {
		delete(event, "breadcrumbs")
	}

	for _, key := range []string{"exception", "threads"} {
		for _, item := range referenceValues(event[key]) {
			if object, ok := item.(map[string]any); ok {
				referenceStacktrace(object["stacktrace"])
			}
		}
	}

	referenceStacktrace(event["stacktrace"])
}

func referenceValues(node any) []any {
	switch typed := node.(type) {
	case []any:
		return typed
	case map[string]any:
		list, _ := typed["values"].([]any)

		return list
	}

	return nil
}

func referenceNewestBreadcrumbs(node any) any {
	trim := func(list []any) []any { return list[max(0, len(list)-50):] }

	switch typed := node.(type) {
	case []any:
		return trim(typed)
	case map[string]any:
		if list, ok := typed["values"].([]any); ok {
			typed["values"] = trim(list)
		}

		return typed
	}

	return node
}

func referenceStacktrace(node any) {
	stacktrace, ok := node.(map[string]any)

	if !ok {
		return
	}

	frames, _ := stacktrace["frames"].([]any)
	hasAppFrames := false

	for _, item := range frames {
		if frame, ok := item.(map[string]any); ok && referenceIsAppFrame(frame) {
			hasAppFrames = true

			break
		}
	}

	if !hasAppFrames {
		return
	}

	for _, item := range frames {
		if frame, ok := item.(map[string]any); ok && !referenceIsAppFrame(frame) {
			for _, key := range []string{"pre_context", "context_line", "post_context", "vars"} {
				delete(frame, key)
			}
		}
	}
}

func referenceIsAppFrame(frame map[string]any) bool {
	filename, _ := frame["filename"].(string)
	absPath, _ := frame["abs_path"].(string)

	return frame["in_app"] == true && !sentry.IsLibraryPath(filename) && !sentry.IsLibraryPath(absPath)
}

var prepared []byte

func BenchmarkPrepareEvent(b *testing.B) {
	for _, name := range []string{"browser-event-handled", "node-event-handled-context", "node-logs"} {
		envelope, err := sentry.ParseEnvelope(fixtures.Envelope(b, name))

		if err != nil {
			b.Fatal(err)
		}

		payload := envelope.Items[0].Payload

		for _, method := range []struct {
			name string
			run  func([]byte) []byte
		}{{"tokens", prepareEvent}, {"tree", referencePrepare}} {
			b.Run(fmt.Sprintf("%s/%s", name, method.name), func(b *testing.B) {
				b.ReportAllocs()
				b.SetBytes(int64(len(payload)))

				for b.Loop() {
					prepared = method.run(payload)
				}
			})
		}
	}
}

// Deep nesting costs time in proportion to the size: an array led by a
// secret's name must not be read again at every level (that grew with the
// square of the depth, a way to stall ingest with one envelope).
func TestDeepNestingStaysLinear(t *testing.T) {
	for name, payload := range map[string]string{
		"pairs":   strings.Repeat(`["token",`, 9000) + "1" + strings.Repeat(`,2]`, 9000),
		"objects": strings.Repeat(`{"session":{"a":`, 4500) + "1" + strings.Repeat(`}}`, 4500),
		"lists":   `{"breadcrumbs":[` + strings.Repeat(`[`, 9000) + strings.Repeat(`]`, 9000) + `]}`,
	} {
		// Against the tree pass on the same input, so a slow machine slows both.
		started := time.Now()

		referencePrepare([]byte(payload))

		tree := time.Since(started)
		started = time.Now()

		prepareEvent([]byte(payload))

		if tokens := time.Since(started); tokens > 10*tree+50*time.Millisecond {
			t.Errorf("%s: %s, the tree pass %s, for %d bytes", name, tokens, tree, len(payload))
		}
	}
}
