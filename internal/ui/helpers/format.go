// Package helpers holds the pure formatting functions of the UI: relative
// times, level colors, log and JSON cleanup, short IDs, retention wording.
// Nothing here knows about HTTP, the store or templates, so every other ui
// package may import it.
package helpers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/sentry"
)

// Short returns the first n characters, for IDs shown in tables.
func Short(text string, n int) string {
	if len(text) <= n {
		return text
	}

	return text[:n]
}

// TimeAgo is a short relative time ("5m ago"). The exact time goes in a title attribute.
func TimeAgo(value time.Time) string {
	elapsed := time.Since(value)

	// A time to come (the next retry of a message) reads "in 4m".
	if elapsed < -time.Minute {
		return "in " + strings.TrimSuffix(TimeAgo(time.Now().Add(elapsed)), " ago") // the same distance back
	}

	switch {
	case elapsed < time.Minute:
		return "just now"
	case elapsed < time.Hour:
		return fmt.Sprintf("%dm ago", int(elapsed.Minutes()))
	case elapsed < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(elapsed.Hours()))
	case elapsed < 30*24*time.Hour:
		return fmt.Sprintf("%dd ago", int(elapsed.Hours()/24))
	default:
		return value.UTC().Format("2006-01-02")
	}
}

// LevelClass maps Sentry and log levels to one of the CSS badge colors.
func LevelClass(level string) string {
	switch strings.ToLower(level) {
	case "fatal", "critical", "error":
		return "level-error"
	case "warning", "warn":
		return "level-warning"
	case "info", "log":
		return "level-info"
	default:
		return "level-debug"
	}
}

var ansiEscape = regexp.MustCompile(`\x1b\[[0-9;]*[A-Za-z]`)

// CleanLog removes terminal color codes that loggers like pino add.
func CleanLog(text string) string {
	return ansiEscape.ReplaceAllString(text, "")
}

// PrettyJSON indents JSON for display; anything else is returned as is.
func PrettyJSON(raw json.RawMessage) string {
	var buffer bytes.Buffer

	if err := json.Indent(&buffer, raw, "", "  "); err != nil {
		return string(raw)
	}

	return buffer.String()
}

func HandledLabel(mechanism *sentry.Mechanism) string {
	if mechanism == nil || mechanism.Handled == nil {
		return ""
	}

	if *mechanism.Handled {
		return "handled"
	}

	return "unhandled"
}

func TruncateRunes(text string, limit int) string {
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit])
	}

	return text
}

// EmailLocalPart is the part of an address before "@": the default name of
// an email connection.
func EmailLocalPart(address string) string {
	local, _, _ := strings.Cut(address, "@")

	return local
}

// MaskSecret shows enough of a stored secret to recognize it, not to use
// it: the start up to the last "/" or ":" of its first part (the webhook
// host and path, the bot id) and the last 4 characters. Empty stays empty.
func MaskSecret(secret string) string {
	runes := []rune(secret)

	if len(runes) == 0 {
		return ""
	}

	if len(runes) <= 8 {
		return "…"
	}

	prefix := 0

	// Up to the last separator in the first half, so the secret part stays hidden.
	for index, char := range runes[:len(runes)/2] {
		if char == '/' || char == ':' || char == '-' {
			prefix = index + 1
		}
	}

	return string(runes[:prefix]) + "…" + string(runes[len(runes)-4:])
}

// CountOrAll shows a count limit: "all" for 0 (no limit).
func CountOrAll(count int) string {
	if count == 0 {
		return "all"
	}

	return strconv.Itoa(count)
}

func KeptFor(duration time.Duration) string {
	switch days := int(duration.Hours() / 24); {
	case duration == 0:
		return "forever"
	case days == 1 && duration == 24*time.Hour:
		return "1 day"
	case days > 1 && duration%(24*time.Hour) == 0:
		return strconv.Itoa(days) + " days"
	default:
		return duration.String()
	}
}
