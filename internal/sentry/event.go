package sentry

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Event is an error or message event. Only the fields the server works with are
// typed; Raw keeps the whole payload so nothing the SDK sent is lost.
// Format: https://develop.sentry.dev/sdk/data-model/event-payloads/
type Event struct {
	EventID     string                     `json:"event_id"`
	Timestamp   Timestamp                  `json:"timestamp"`
	Level       string                     `json:"level"`
	Platform    string                     `json:"platform"`
	Logger      string                     `json:"logger"`
	Environment string                     `json:"environment"`
	Release     string                     `json:"release"`
	ServerName  string                     `json:"server_name"`
	Transaction string                     `json:"transaction"`
	Message     Message                    `json:"message"`
	LogEntry    Message                    `json:"logentry"`
	Exception   ExceptionList              `json:"exception"`
	Fingerprint StringList                 `json:"fingerprint"`
	Tags        Tags                       `json:"tags"`
	User        *User                      `json:"user"`
	Contexts    map[string]json.RawMessage `json:"contexts"`
	Breadcrumbs BreadcrumbList             `json:"breadcrumbs"`
	Request     json.RawMessage            `json:"request"`
	SDK         *SDK                       `json:"sdk"`

	Raw json.RawMessage `json:"-"`
}

type Exception struct {
	Type       string      `json:"type"`
	Value      string      `json:"value"`
	Module     string      `json:"module"`
	Mechanism  *Mechanism  `json:"mechanism"`
	Stacktrace *Stacktrace `json:"stacktrace"`
}

type Mechanism struct {
	Type    string `json:"type"`
	Handled *bool  `json:"handled"`
}

type Stacktrace struct {
	// Frames are ordered oldest first: the frame that threw is last.
	Frames []Frame `json:"frames"`
}

type Frame struct {
	Filename    string          `json:"filename"`
	AbsPath     string          `json:"abs_path"`
	Module      string          `json:"module"`
	Function    string          `json:"function"`
	Lineno      int             `json:"lineno"`
	Colno       int             `json:"colno"`
	InApp       *bool           `json:"in_app"`
	ContextLine string          `json:"context_line"`
	PreContext  []string        `json:"pre_context"`
	PostContext []string        `json:"post_context"`
	Vars        json.RawMessage `json:"vars"`
}

type User struct {
	ID        FlexString `json:"id"`
	Email     string     `json:"email"`
	Username  string     `json:"username"`
	IPAddress string     `json:"ip_address"`
}

type Breadcrumb struct {
	Timestamp Timestamp       `json:"timestamp"`
	Type      string          `json:"type"`
	Category  string          `json:"category"`
	Level     string          `json:"level"`
	Message   string          `json:"message"`
	Data      json.RawMessage `json:"data"`
}

// ParseEvent decodes an event item payload. headerEventID is the envelope's
// event_id, used when the payload has none. Missing IDs, timestamps and levels
// get defaults, so the result is always usable.
func ParseEvent(payload []byte, headerEventID string, received time.Time) (*Event, error) {
	event := &Event{}
	if err := json.Unmarshal(payload, event); err != nil {
		return nil, fmt.Errorf("event: %w", err)
	}
	event.Raw = json.RawMessage(bytes.Clone(payload))

	event.EventID = normalizeEventID(event.EventID)
	if event.EventID == "" {
		event.EventID = normalizeEventID(headerEventID)
	}
	if event.EventID == "" {
		event.EventID = NewEventID()
	}
	if event.Timestamp.IsZero() {
		event.Timestamp = Timestamp{received}
	}
	if event.Level == "" {
		event.Level = "error"
	}
	if event.Message == "" {
		event.Message = event.LogEntry
	}
	return event, nil
}

// Title is a one-line summary: "Type: value" of the outermost exception, or the message.
func (event *Event) Title() string {
	if exception := event.MainException(); exception != nil {
		switch {
		case exception.Type != "" && exception.Value != "":
			return exception.Type + ": " + exception.Value
		case exception.Type != "":
			return exception.Type
		case exception.Value != "":
			return exception.Value
		}
	}
	if event.Message != "" {
		return string(event.Message)
	}
	return "<unlabeled event>"
}

// MainException is the outermost exception. In a cause chain the SDK sends
// the innermost cause first, so it is the last one.
func (event *Event) MainException() *Exception {
	if len(event.Exception) == 0 {
		return nil
	}
	return &event.Exception[len(event.Exception)-1]
}

// TraceID returns contexts.trace.trace_id, which links the event to logs.
func (event *Event) TraceID() string {
	raw, ok := event.Contexts["trace"]
	if !ok {
		return ""
	}
	var trace struct {
		TraceID string `json:"trace_id"`
	}
	_ = json.Unmarshal(raw, &trace)
	return trace.TraceID
}

// NewEventID returns a random 32-character hex ID in Sentry's format.
func NewEventID() string {
	var id [16]byte
	_, _ = rand.Read(id[:])
	return hex.EncodeToString(id[:])
}

// normalizeEventID accepts both "abcd..." and UUID form with dashes, and
// returns 32 lowercase hex characters, or "" if the value is not a valid ID.
func normalizeEventID(value string) string {
	value = strings.ToLower(strings.ReplaceAll(value, "-", ""))
	if len(value) != 32 {
		return ""
	}
	if _, err := hex.DecodeString(value); err != nil {
		return ""
	}
	return value
}

// The types below accept every shape the Sentry protocol allows for a field.
// On an unexpected shape they keep the zero value instead of rejecting the whole
// event: losing one field is better than losing the error report.

// Timestamp accepts Unix seconds (float) or an RFC 3339 string.
type Timestamp struct{ time.Time }

func (timestamp *Timestamp) UnmarshalJSON(data []byte) error {
	var seconds float64
	if err := json.Unmarshal(data, &seconds); err == nil {
		whole := int64(seconds)
		timestamp.Time = time.Unix(whole, int64((seconds-float64(whole))*1e9)).UTC()
		return nil
	}
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		for _, layout := range []string{time.RFC3339Nano, "2006-01-02T15:04:05.999999999"} {
			if parsed, err := time.Parse(layout, text); err == nil {
				timestamp.Time = parsed.UTC()
				return nil
			}
		}
	}
	return nil
}

// Message accepts a plain string or {"formatted": ..., "message": ..., "params": [...]}.
type Message string

func (message *Message) UnmarshalJSON(data []byte) error {
	var text string
	if err := json.Unmarshal(data, &text); err == nil {
		*message = Message(text)
		return nil
	}
	var object struct {
		Formatted string `json:"formatted"`
		Message   string `json:"message"`
	}
	if err := json.Unmarshal(data, &object); err == nil {
		if object.Formatted != "" {
			*message = Message(object.Formatted)
		} else {
			*message = Message(object.Message)
		}
	}
	return nil
}

// ExceptionList accepts {"values": [...]} or a bare list.
type ExceptionList []Exception

func (list *ExceptionList) UnmarshalJSON(data []byte) error {
	return unmarshalValues(data, (*[]Exception)(list))
}

// BreadcrumbList accepts {"values": [...]} or a bare list.
type BreadcrumbList []Breadcrumb

func (list *BreadcrumbList) UnmarshalJSON(data []byte) error {
	return unmarshalValues(data, (*[]Breadcrumb)(list))
}

func unmarshalValues[T any](data []byte, target *[]T) error {
	var wrapped struct {
		Values []T `json:"values"`
	}
	if err := json.Unmarshal(data, &wrapped); err == nil {
		*target = wrapped.Values
		return nil
	}
	var bare []T
	if err := json.Unmarshal(data, &bare); err == nil {
		*target = bare
	}
	return nil
}

// Tags accepts {"key": "value"} or [["key", "value"], ...]. Non-string values are stringified.
type Tags map[string]string

func (tags *Tags) UnmarshalJSON(data []byte) error {
	result := Tags{}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(data, &object); err == nil {
		for key, value := range object {
			result[key] = rawToString(value)
		}
		*tags = result
		return nil
	}
	var pairs [][]json.RawMessage
	if err := json.Unmarshal(data, &pairs); err == nil {
		for _, pair := range pairs {
			if len(pair) == 2 {
				result[rawToString(pair[0])] = rawToString(pair[1])
			}
		}
		*tags = result
	}
	return nil
}

// StringList is a list of strings where numbers and booleans are stringified.
type StringList []string

func (list *StringList) UnmarshalJSON(data []byte) error {
	var values []json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return nil
	}
	result := make(StringList, 0, len(values))
	for _, value := range values {
		result = append(result, rawToString(value))
	}
	*list = result
	return nil
}

// FlexString accepts a string or a number (user.id is often numeric).
type FlexString string

func (flex *FlexString) UnmarshalJSON(data []byte) error {
	*flex = FlexString(rawToString(data))
	return nil
}

func rawToString(raw json.RawMessage) string {
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	var boolean bool
	if err := json.Unmarshal(raw, &boolean); err == nil {
		return strconv.FormatBool(boolean)
	}
	if string(raw) == "null" {
		return ""
	}
	return string(raw)
}
