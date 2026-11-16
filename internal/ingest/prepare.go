package ingest

import (
	"bytes"
	"encoding/json"

	"github.com/devforth/exception-monitoring/internal/ingest/compact"
	"github.com/devforth/exception-monitoring/internal/ingest/scrub"
)

// prepareEvent removes credentials (package scrub) and what does not help a
// fix (package compact) before typed parsing, in one JSON pass: the
// decoded tree and the encoded payload are allocated once for both.
// Invalid input is left for the event parser to reject, as in scrub.JSON.
func prepareEvent(payload []byte) []byte {
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.UseNumber()

	var document any

	if err := decoder.Decode(&document); err != nil {
		return payload
	}

	document = scrub.Value(document)

	if event, ok := document.(map[string]any); ok && event != nil {
		compact.EventFields(event)
	}

	var output bytes.Buffer

	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)

	if err := encoder.Encode(document); err != nil {
		return payload
	}

	return bytes.TrimRight(output.Bytes(), "\n")
}
