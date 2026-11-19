package ingest

import (
	"bytes"

	"github.com/devforth/exception-monitoring/internal/ingest/compact"
	"github.com/devforth/exception-monitoring/internal/ingest/scrub"
)

// prepareEvent removes credentials (package scrub) and what does not help a
// fix (package compact) in one pass over the JSON tokens, with no tree built.
// Invalid input is left for the event parser to reject, as in scrub.JSON.
func prepareEvent(payload []byte) []byte {
	var output bytes.Buffer

	if err := compact.Event(scrub.NewDecoder(payload), scrub.NewEncoder(&output)); err != nil {
		return payload
	}

	return bytes.TrimRight(output.Bytes(), "\n")
}
