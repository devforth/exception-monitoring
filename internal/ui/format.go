package ui

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"regexp"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/sentry"
)

var templateFuncs = template.FuncMap{
	"timeAgo":      timeAgo,
	"isoTime":      func(value time.Time) string { return value.UTC().Format(time.RFC3339) },
	"fullTime":     func(value time.Time) string { return value.UTC().Format("2006-01-02 15:04:05.000 UTC") },
	"levelClass":   levelClass,
	"cleanLog":     cleanLog,
	"prettyJSON":   prettyJSON,
	"handledLabel": handledLabel,
	"parseRequest": parseRequest,
	"short":        short,
	"asset":        assetURL,
	// filterBy passes a filter link and one value to the level-filter / project-filter partials.
	"filterBy": func(links filterLinks, field, value string) map[string]any {
		return map[string]any{"Filter": links, field: value}
	},
}

// short returns the first n characters, for IDs shown in tables.
func short(text string, n int) string {
	if len(text) <= n {
		return text
	}

	return text[:n]
}

// timeAgo is a short relative time ("5m ago"). The exact time goes in a title attribute.
func timeAgo(value time.Time) string {
	elapsed := time.Since(value)

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

// levelClass maps Sentry and log levels to one of the CSS badge colors.
func levelClass(level string) string {
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

// cleanLog removes terminal color codes that loggers like pino add.
func cleanLog(text string) string {
	return ansiEscape.ReplaceAllString(text, "")
}

// prettyJSON indents JSON for display; anything else is returned as is.
func prettyJSON(raw json.RawMessage) string {
	var buffer bytes.Buffer

	if err := json.Indent(&buffer, raw, "", "  "); err != nil {
		return string(raw)
	}

	return buffer.String()
}

func handledLabel(mechanism *sentry.Mechanism) string {
	if mechanism == nil || mechanism.Handled == nil {
		return ""
	}

	if *mechanism.Handled {
		return "handled"
	}

	return "unhandled"
}

type requestInfo struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Query   json.RawMessage   `json:"query_string"`
	Headers map[string]string `json:"headers"`
	Data    json.RawMessage   `json:"data"`
}

// parseRequest decodes the event's "request" context. Headers may come as an
// object or as [name, value] pairs; pairs are converted.
func parseRequest(raw json.RawMessage) *requestInfo {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	var info requestInfo

	if err := json.Unmarshal(raw, &info); err != nil {
		var loose struct {
			Method  string          `json:"method"`
			URL     string          `json:"url"`
			Headers [][2]string     `json:"headers"`
			Query   json.RawMessage `json:"query_string"`
			Data    json.RawMessage `json:"data"`
		}

		if json.Unmarshal(raw, &loose) != nil {
			return nil
		}

		info = requestInfo{Method: loose.Method, URL: loose.URL, Query: loose.Query, Data: loose.Data, Headers: map[string]string{}}

		for _, pair := range loose.Headers {
			info.Headers[pair[0]] = pair[1]
		}
	}

	return &info
}
