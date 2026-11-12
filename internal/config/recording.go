package config

import (
	"fmt"
	"slices"
	"strings"
)

// Recording: which levels are stored. Events and logs of other levels are
// dropped at ingest. A nil list means every level.
type Recording struct {
	EventLevels []string
	LogLevels   []string
}

// LogLevels are the log levels of the Sentry logs protocol, lowest first.
var LogLevels = []string{"trace", "debug", "info", "warn", "error", "fatal"}

// DefaultRecordEventLevels: errors only by default; messages and warnings
// are recorded when a project asks for them.
var DefaultRecordEventLevels = []string{"error", "fatal"}

func parseRecording(getenv func(string) string) (Recording, error) {
	recording := Recording{EventLevels: DefaultRecordEventLevels}

	for _, setting := range []struct {
		name   string
		known  []string
		target *[]string
	}{
		{"RECORD_EVENT_LEVELS", Levels, &recording.EventLevels},
		{"RECORD_LOG_LEVELS", LogLevels, &recording.LogLevels},
	} {
		value := strings.TrimSpace(getenv(setting.name))

		if value == "" {
			continue
		}

		levels, err := ParseLevels(value, setting.known)

		if err != nil {
			return recording, fmt.Errorf("%s: %w", setting.name, err)
		}

		*setting.target = levels
	}

	return recording, nil
}

// ParseLevels reads "error,fatal" against the known levels; "all" is nil
// (every level), "none" an empty list.
func ParseLevels(value string, known []string) ([]string, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "all":
		return nil, nil
	case "none":
		return []string{}, nil
	}

	levels := []string{}

	for entry := range strings.SplitSeq(value, ",") {
		level := strings.ToLower(strings.TrimSpace(entry))

		if !slices.Contains(known, level) {
			return nil, fmt.Errorf("unknown level %q (expected %s, all or none)", level, strings.Join(known, ", "))
		}

		if !slices.Contains(levels, level) {
			levels = append(levels, level)
		}
	}

	return levels, nil
}

// Records reports whether a level is in the list; nil records every level.
func Records(levels []string, level string) bool {
	return levels == nil || slices.Contains(levels, level)
}

// EventLevelsFor and LogLevelsFor give the levels a project records: its
// own list (Project.EventLevels, LogLevels), or nil for these defaults.
func (recording Recording) EventLevelsFor(own *[]string) []string {
	return ownOr(own, recording.EventLevels)
}

func (recording Recording) LogLevelsFor(own *[]string) []string {
	return ownOr(own, recording.LogLevels)
}

func ownOr(own *[]string, defaults []string) []string {
	if own != nil {
		return *own
	}

	return defaults
}

// EventLevel maps a level to one of Levels: "critical" is fatal, "warn"
// warning, "log" info, "trace" debug (so log levels map too), anything else
// unknown counts as error. Ingest stores events with it, so the database
// and everything reading it know only these names.
func EventLevel(level string) string {
	switch level = strings.ToLower(level); level {
	case "critical":
		return "fatal"
	case "warn":
		return "warning"
	case "log":
		return "info"
	case "trace":
		return "debug"
	}

	if slices.Contains(Levels, level) {
		return level
	}

	return "error"
}

// LogLevel maps a log level to one of LogLevels ("warning" is warn,
// "critical" fatal; anything else unknown counts as info). Ingest stores
// logs with it.
func LogLevel(level string) string {
	switch level = strings.ToLower(level); level {
	case "warning":
		return "warn"
	case "critical":
		return "fatal"
	}

	if slices.Contains(LogLevels, level) {
		return level
	}

	return "info"
}
