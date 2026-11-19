// Package sentry parses the Sentry SDK wire format: envelopes, events and logs.
// It knows nothing about HTTP or storage.
package sentry

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
)

// MaxItems caps the number of items in one envelope. Real SDKs send one or two.
const MaxItems = 100

// Envelope is a parsed envelope: one header line followed by items.
// Format: https://develop.sentry.dev/sdk/data-model/envelopes/
type Envelope struct {
	Header EnvelopeHeader
	Items  []Item
}

type EnvelopeHeader struct {
	EventID string `json:"event_id"`
	SentAt  string `json:"sent_at"`
	// DSN is set when the SDK sends through a tunnel, which has no auth in the request itself.
	DSN string `json:"dsn"`
}

type SDK struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type Item struct {
	Header  ItemHeader
	Payload []byte
}

type ItemHeader struct {
	Type   string `json:"type"`
	Length *int   `json:"length"`
}

// Item types the server stores. Everything else (span, transaction, session,
// client_report, attachment, replay, ...) is accepted and dropped.
const (
	ItemEvent = "event"
	ItemLog   = "log"
)

// ParseEnvelope splits an envelope body into its header and items.
//
// Each item is a header line followed by a payload. If the item header has
// "length", the payload is exactly that many bytes (it may contain newlines or
// binary data); otherwise it runs to the next newline or the end of the body.
func ParseEnvelope(body []byte) (*Envelope, error) {
	headerLine, rest := nextLine(body)

	if len(bytes.TrimSpace(headerLine)) == 0 {
		return nil, errors.New("empty envelope")
	}

	envelope := &Envelope{}

	if err := json.Unmarshal(headerLine, &envelope.Header); err != nil {
		return nil, fmt.Errorf("envelope header: %w", err)
	}

	for len(rest) > 0 {
		var itemHeaderLine []byte

		itemHeaderLine, rest = nextLine(rest)

		if len(bytes.TrimSpace(itemHeaderLine)) == 0 {
			// Trailing newlines at the end of the body.
			if len(bytes.TrimSpace(rest)) == 0 {
				break
			}

			return nil, fmt.Errorf("item %d: empty item header", len(envelope.Items)+1)
		}

		if len(envelope.Items) == MaxItems {
			return nil, fmt.Errorf("too many items (max %d)", MaxItems)
		}

		var item Item

		if err := json.Unmarshal(itemHeaderLine, &item.Header); err != nil {
			return nil, fmt.Errorf("item %d header: %w", len(envelope.Items)+1, err)
		}

		if item.Header.Length != nil {
			length := *item.Header.Length

			if length < 0 || length > len(rest) {
				return nil, fmt.Errorf("item %d: length %d does not fit the body", len(envelope.Items)+1, length)
			}

			item.Payload = rest[:length]
			rest = rest[length:]
			// The payload may be followed by a newline separator.
			if len(rest) > 0 && rest[0] == '\n' {
				rest = rest[1:]
			}
		} else {
			item.Payload, rest = nextLine(rest)
		}

		envelope.Items = append(envelope.Items, item)
	}

	return envelope, nil
}

// nextLine returns the bytes up to the next '\n' (without it) and the remainder.
func nextLine(data []byte) (line, rest []byte) {
	if index := bytes.IndexByte(data, '\n'); index >= 0 {
		return bytes.TrimSuffix(data[:index], []byte("\r")), data[index+1:]
	}

	return data, nil
}
