package notify

import (
	"encoding/json"
	"fmt"
	"strings"
)

// Shared wording of the short formats (Slack, Telegram). Email has its own
// long format in email.go.

func (kind Kind) label() string {
	switch kind {
	case Regression:
		return "Regression"
	case Test:
		return "Test message"
	default:
		return "New issue"
	}
}

// details is the second line of a short message: where and in which environment.
func (notification Notification) details() string {
	var parts []string

	for _, part := range []string{notification.Culprit, notification.Environment, notification.Release} {
		if part != "" {
			parts = append(parts, part)
		}
	}

	return strings.Join(parts, " · ")
}

func (summary Summary) heading() string {
	return fmt.Sprintf("%d more issues in the last %s", summary.Count, summary.Window.Round(1e9))
}

// note explains why the issues were not sent one by one.
func (summary Summary) note() string {
	more := summary.Count - len(summary.Items)
	note := "Sent together to avoid flooding the chat."

	if more > 0 {
		note = fmt.Sprintf("…and %d more. %s", more, note)
	}

	return note
}

// truncate cuts text to at most limit characters (not bytes), adding "…".
func truncate(text string, limit int) string {
	runes := []rune(text)

	if len(runes) <= limit {
		return text
	}

	return string(runes[:limit-1]) + "…"
}

// Description is what the delivery log shows of an outbox message.
type Description struct {
	Label        string // "New issue", "Regression", "Test message" or "Summary"
	Project      string
	ProjectColor string
	Title        string // the issue; for a summary, how many issues it covers
	IssueID      int64
}

// Describe reads an outbox message (kind "notification" or "summary") for
// display. A payload that does not parse gives what could be read.
func Describe(kind string, payload []byte) Description {
	if kind == kindSummary {
		var summary Summary

		_ = json.Unmarshal(payload, &summary)

		return Description{Label: "Summary", Title: summary.heading()}
	}

	var notification Notification

	_ = json.Unmarshal(payload, &notification)

	return Description{
		Label: notification.Kind.label(), Project: notification.Project, ProjectColor: notification.ProjectColor,
		Title: notification.Title, IssueID: notification.IssueID,
	}
}
