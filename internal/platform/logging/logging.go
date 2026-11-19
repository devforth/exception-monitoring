// Package logging builds the server's logger. In a terminal it writes
// lines for people (Pretty): a short time, the level in color, the message
// and its fields aligned. Elsewhere (Docker, log collectors) it keeps the
// machine formats of log/slog: logfmt text or JSON. LOG_FORMAT chooses.
package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"
)

// Formats of LOG_FORMAT.
const (
	FormatAuto   = "auto" // pretty in a terminal, text otherwise
	FormatPretty = "pretty"
	FormatText   = "text"
	FormatJSON   = "json"
)

// Formats lists the valid LOG_FORMAT values.
var Formats = []string{FormatAuto, FormatPretty, FormatText, FormatJSON}

// Block marks a record of settings: the pretty format writes its fields
// one per line, whatever the width. Other formats leave the mark out.
var Block = slog.Bool(blockKey, true)

const blockKey = "_block"

// New returns a logger writing to file in format at level.
func New(file *os.File, format string, level slog.Leveler) *slog.Logger {
	options := &slog.HandlerOptions{Level: level, ReplaceAttr: func(_ []string, attr slog.Attr) slog.Attr {
		if attr.Key == blockKey {
			return slog.Attr{} // dropped
		}

		return attr
	}}

	if format == FormatAuto || format == "" {
		format = FormatText

		if isTerminal(file) {
			format = FormatPretty
		}
	}

	switch format {
	case FormatJSON:
		return slog.New(slog.NewJSONHandler(file, options))
	case FormatPretty:
		pretty := NewPretty(file, level, isTerminal(file) && os.Getenv("NO_COLOR") == "")
		pretty.width = func() int { return terminalWidth(file) }

		return slog.New(pretty)
	default:
		return slog.New(slog.NewTextHandler(file, options))
	}
}

// isTerminal reports whether file is a terminal (a character device), not a
// pipe or a file.
func isTerminal(file *os.File) bool {
	info, err := file.Stat()

	return err == nil && info.Mode()&os.ModeCharDevice != 0
}

// Pretty is a slog handler for reading in a terminal; a Block record or a
// line wider than the terminal puts each field on a line of its own.
type Pretty struct {
	out   io.Writer
	level slog.Leveler
	color bool
	// width gives the terminal's columns when the line is written (it may
	// be resized); nil or 0: lines are never wrapped.
	width  func() int
	attrs  []slog.Attr // from WithAttrs, with their group prefix
	group  string      // from WithGroup, as "group."
	writer *sync.Mutex // shared by the handlers derived from one
}

func NewPretty(out io.Writer, level slog.Leveler, color bool) *Pretty {
	if level == nil {
		level = slog.LevelInfo
	}

	return &Pretty{out: out, level: level, color: color, writer: &sync.Mutex{}}
}

// messageWidth aligns the fields of short messages.
const messageWidth = 28

const (
	dim    = "\033[2m"
	bold   = "\033[1m"
	red    = "\033[31m"
	yellow = "\033[33m"
	green  = "\033[32m"
	blue   = "\033[34m"
	reset  = "\033[0m"
)

func (handler *Pretty) Enabled(_ context.Context, level slog.Level) bool {
	return level >= handler.level.Level()
}

func (handler *Pretty) Handle(_ context.Context, record slog.Record) error {
	var line strings.Builder

	line.WriteString(handler.paint(dim, record.Time.Format(time.TimeOnly)) + " ")
	line.WriteString(handler.paint(levelColor(record.Level), fmt.Sprintf("%-5s", record.Level.String())) + " ")

	// What the fields are indented by on lines of their own: under the message.
	const indent = len("15:04:05 LEVEL ")

	fields := slices.Clone(handler.attrs)

	record.Attrs(func(attr slog.Attr) bool {
		fields = append(fields, prefixed(handler.group, attr)...)

		return true
	})

	type field struct{ key, value string }

	var shown []field

	length := indent + utf8.RuneCountInString(record.Message)
	block := false

	for _, attr := range fields {
		if attr.Key == blockKey {
			block = true

			continue
		}

		if value := attr.Value.Resolve().String(); value != "" {
			shown = append(shown, field{attr.Key, value})
			length += 2 + utf8.RuneCountInString(attr.Key) + 1 + utf8.RuneCountInString(value)
		}
	}

	message := record.Message
	wrap := block || handler.width != nil && handler.width() > 0 && max(length, indent+messageWidth) > handler.width()

	if len(shown) > 0 && !wrap && len(message) < messageWidth {
		message += strings.Repeat(" ", messageWidth-len(message))
	}

	line.WriteString(handler.paint(messageStyle(record.Level), message))

	for _, field := range shown {
		if field.key == "err" {
			field.value = handler.paint(red, field.value)
		}

		separator := "  "

		if wrap {
			separator = "\n" + strings.Repeat(" ", indent)
		}

		line.WriteString(separator + handler.paint(dim, field.key+"=") + field.value)
	}

	line.WriteString("\n")

	handler.writer.Lock()
	defer handler.writer.Unlock()

	_, err := io.WriteString(handler.out, line.String())

	return err
}

func (handler *Pretty) WithAttrs(attrs []slog.Attr) slog.Handler {
	derived := *handler

	derived.attrs = slices.Clone(handler.attrs)

	for _, attr := range attrs {
		derived.attrs = append(derived.attrs, prefixed(handler.group, attr)...)
	}

	return &derived
}

func (handler *Pretty) WithGroup(name string) slog.Handler {
	if name == "" {
		return handler
	}

	derived := *handler
	derived.group = handler.group + name + "."

	return &derived
}

// prefixed flattens an attribute (groups become dotted keys).
func prefixed(group string, attr slog.Attr) []slog.Attr {
	if attr.Value.Kind() != slog.KindGroup {
		return []slog.Attr{{Key: group + attr.Key, Value: attr.Value}}
	}

	var flat []slog.Attr

	for _, member := range attr.Value.Group() {
		flat = append(flat, prefixed(group+attr.Key+".", member)...)
	}

	return flat
}

func (handler *Pretty) paint(style, text string) string {
	if !handler.color || style == "" {
		return text
	}

	return style + text + reset
}

func levelColor(level slog.Level) string {
	switch {
	case level >= slog.LevelError:
		return red + bold
	case level >= slog.LevelWarn:
		return yellow + bold
	case level >= slog.LevelInfo:
		return green
	default:
		return blue
	}
}

func messageStyle(level slog.Level) string {
	if level >= slog.LevelWarn {
		return bold
	}

	return ""
}
