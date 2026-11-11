package sentry

import (
	"encoding/json"
	"fmt"
	"time"
)

// Log is one structured log record from a "log" item.
// Format: https://develop.sentry.dev/sdk/telemetry/logs/
type Log struct {
	Timestamp      Timestamp            `json:"timestamp"`
	Level          string               `json:"level"`
	Body           string               `json:"body"`
	TraceID        string               `json:"trace_id"`
	SpanID         string               `json:"span_id"`
	SeverityNumber int                  `json:"severity_number"`
	Attributes     map[string]Attribute `json:"attributes"`
}

// Attribute is a typed log attribute: Type is "string", "integer", "double" or "boolean".
type Attribute struct {
	Type  string          `json:"type"`
	Value json.RawMessage `json:"value"`
}

// String returns the attribute value as text.
func (attribute Attribute) String() string {
	return rawToString(attribute.Value)
}

// ParseLogs decodes a log item payload: {"items": [...]}.
func ParseLogs(payload []byte, received time.Time) ([]Log, error) {
	var batch struct {
		Items []Log `json:"items"`
	}

	if err := json.Unmarshal(payload, &batch); err != nil {
		return nil, fmt.Errorf("logs: %w", err)
	}

	for index := range batch.Items {
		record := &batch.Items[index]

		if record.Timestamp.IsZero() {
			record.Timestamp = Timestamp{received}
		}

		if record.Level == "" {
			record.Level = "info"
		}
	}

	return batch.Items, nil
}
