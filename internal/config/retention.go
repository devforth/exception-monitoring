package config

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Retention limits how much data is kept. A zero value disables that limit.
type Retention struct {
	// Events older than this are deleted (by the time the server received them).
	Events time.Duration
	// Logs older than this are deleted. Logs usually outnumber events many
	// times, so they are kept for a shorter time by default.
	Logs time.Duration
	// EventsPerIssue keeps only the newest events of every issue; the issue's
	// event count still counts all of them.
	EventsPerIssue int
	// LogsPerMessage keeps only the newest logs of each repeated message (the
	// same text with other numbers or IDs), so a line logged in a loop cannot
	// fill the database.
	LogsPerMessage int
	// MaxDatabaseBytes: above it, the oldest events and logs are deleted.
	MaxDatabaseBytes int64
	// Issues not seen for this long are deleted with their events. Until
	// then an issue is never deleted and keeps its latest event.
	Issues time.Duration
}

// RetentionOverride is a project's own retention; nil fields use the global value.
type RetentionOverride struct {
	Events         *time.Duration
	Logs           *time.Duration
	Issues         *time.Duration
	EventsPerIssue *int
	LogsPerMessage *int
}

// For returns the retention of a project: its overrides on top of these settings.
func (retention Retention) For(override RetentionOverride) Retention {
	if override.Events != nil {
		retention.Events = *override.Events
	}

	if override.Logs != nil {
		retention.Logs = *override.Logs
	}

	if override.Issues != nil {
		retention.Issues = *override.Issues
	}

	if override.EventsPerIssue != nil {
		retention.EventsPerIssue = *override.EventsPerIssue
	}

	if override.LogsPerMessage != nil {
		retention.LogsPerMessage = *override.LogsPerMessage
	}

	return retention
}

// Retention defaults.
const (
	DefaultRetentionEvents         = 30 * 24 * time.Hour
	DefaultRetentionLogs           = 14 * 24 * time.Hour
	DefaultRetentionEventsPerIssue = 100
	DefaultMaxDatabaseBytes        = 2 << 30 // 2 GB
	DefaultRetentionIssues         = 365 * 24 * time.Hour
	DefaultRetentionLogsPerMessage = 50
)

func parseRetention(getenv func(string) string) (Retention, error) {
	retention := Retention{
		Events:           DefaultRetentionEvents,
		Logs:             DefaultRetentionLogs,
		EventsPerIssue:   DefaultRetentionEventsPerIssue,
		MaxDatabaseBytes: DefaultMaxDatabaseBytes,
		Issues:           DefaultRetentionIssues,
		LogsPerMessage:   DefaultRetentionLogsPerMessage,
	}

	for _, setting := range []struct {
		name   string
		target *time.Duration
	}{
		{"RETENTION_EVENTS", &retention.Events},
		{"RETENTION_LOGS", &retention.Logs},
		{"RETENTION_ISSUES", &retention.Issues},
	} {
		if value := strings.TrimSpace(getenv(setting.name)); value != "" {
			duration, err := ParseOptionalDuration(value)

			if err != nil {
				return retention, fmt.Errorf("%s: %w", setting.name, err)
			}

			*setting.target = duration
		}
	}

	for _, setting := range []struct {
		name   string
		target *int
	}{
		{"RETENTION_EVENTS_PER_ISSUE", &retention.EventsPerIssue},
		{"RETENTION_LOGS_PER_MESSAGE", &retention.LogsPerMessage},
	} {
		if value := strings.TrimSpace(getenv(setting.name)); value != "" {
			count, err := ParseCount(value)

			if err != nil {
				return retention, fmt.Errorf("%s: %w", setting.name, err)
			}

			*setting.target = count
		}
	}

	if value := strings.TrimSpace(getenv("MAX_DATABASE_SIZE")); value != "" {
		size, err := parseSize(value)

		if err != nil {
			return retention, fmt.Errorf("MAX_DATABASE_SIZE: %w", err)
		}

		retention.MaxDatabaseBytes = size
	}

	return retention, nil
}

// ParseCount reads a limit: 0 (no limit) or a positive integer.
func ParseCount(value string) (int, error) {
	count, err := strconv.Atoi(strings.TrimSpace(value))

	if err != nil || count < 0 {
		return 0, fmt.Errorf("invalid value %q (0 or a positive integer)", value)
	}

	return count, nil
}

// FormatDuration writes a duration the way the settings take it ("30d",
// "12h", "1h30m", "0"): the inverse of ParseOptionalDuration.
func FormatDuration(duration time.Duration) string {
	if duration == 0 {
		return "0"
	}

	if duration%(24*time.Hour) == 0 {
		return strconv.Itoa(int(duration/(24*time.Hour))) + "d"
	}

	// "12h0m0s" → "12h", "1h30m0s" → "1h30m".
	text := duration.String()

	if strings.HasSuffix(text, "m0s") {
		text = strings.TrimSuffix(text, "0s")
	}

	if strings.HasSuffix(text, "h0m") {
		text = strings.TrimSuffix(text, "0m")
	}

	return text
}

// ParseOptionalDuration is parseDuration that also accepts "0" (no limit).
func ParseOptionalDuration(value string) (time.Duration, error) {
	if value == "0" {
		return 0, nil
	}

	return parseDuration(value)
}

var sizeUnits = []struct {
	suffix string
	bytes  int64
}{
	{"TB", 1 << 40},
	{"GB", 1 << 30},
	{"MB", 1 << 20},
	{"KB", 1 << 10},
	{"B", 1},
}

// parseSize reads "500MB", "2GB" or "1.5GB" (binary units: 1 GB = 1024 MB);
// "0" means no limit.
func parseSize(value string) (int64, error) {
	if value == "0" {
		return 0, nil
	}

	upper := strings.ToUpper(strings.ReplaceAll(value, " ", ""))

	for _, unit := range sizeUnits {
		if number, ok := strings.CutSuffix(upper, unit.suffix); ok {
			amount, err := strconv.ParseFloat(number, 64)

			if err != nil || amount <= 0 {
				break
			}

			return int64(amount * float64(unit.bytes)), nil
		}
	}

	return 0, fmt.Errorf("invalid size %q (e.g. 500MB, 2GB, or 0 for no limit)", value)
}

// FormatSize shows a byte count the way MAX_DATABASE_SIZE is written.
func FormatSize(bytes int64) string {
	for _, unit := range sizeUnits {
		if bytes >= unit.bytes && unit.bytes > 1 {
			return strconv.FormatFloat(float64(bytes)/float64(unit.bytes), 'f', 1, 64) + " " + unit.suffix
		}
	}

	return strconv.FormatInt(bytes, 10) + " B"
}
