package helpers

import (
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
)

// RetentionDifferences lists a project's own retention, e.g. "logs 3 days".
func RetentionDifferences(global config.Retention, override config.RetentionOverride) string {
	own := global.For(override)

	var parts []string

	for _, item := range []struct {
		label        string
		value, other time.Duration
	}{
		{"events", own.Events, global.Events},
		{"logs", own.Logs, global.Logs},
		{"issues", own.Issues, global.Issues},
	} {
		if item.value != item.other {
			parts = append(parts, item.label+" "+KeptFor(item.value))
		}
	}

	if own.EventsPerIssue != global.EventsPerIssue {
		parts = append(parts, CountOrAll(own.EventsPerIssue)+" events per issue")
	}

	if own.LogsPerMessage != global.LogsPerMessage {
		parts = append(parts, CountOrAll(own.LogsPerMessage)+" logs per message")
	}

	return strings.Join(parts, ", ")
}

// DurationField writes an optional duration the way the settings take it
// ("30d", "12h", "0"); empty when not set.
func DurationField(duration *time.Duration) string {
	if duration == nil {
		return ""
	}

	return config.FormatDuration(*duration)
}

// CountField writes an optional count; empty when not set.
func CountField(count *int) string {
	if count == nil {
		return ""
	}

	return strconv.Itoa(*count)
}
