// Package grouping decides which events are the same problem (issue).
//
// Every event gets a fingerprint; events with the same fingerprint in the same
// project belong to one issue. The fingerprint is a hash of "components":
//
//  1. If the SDK sent a custom fingerprint, its values are the components.
//     "{{ default }}" inside it expands to the default components.
//  2. Otherwise, for exceptions: each exception in the chain contributes its
//     type and its stack frames (only in-app frames if there are any). A frame
//     is the file (or module), the function and the source line of the call
//     (context_line), without line numbers: a deploy that shifts code by a few
//     lines does not create a new issue, while two throws in one function, or
//     two functions with the same name (JS names `handler: () => {}` after the
//     property), stay apart. Anonymous functions without source fall back to
//     the line number. If no frame has source code (browser SDKs send none),
//     the stack says little, so the normalized error text is added too.
//  3. Events without a usable stack (messages, exceptions without frames) are
//     grouped by their text with variable parts (numbers, IDs, hashes)
//     replaced by placeholders, so "order 890" and "order 891" match.
package grouping

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"strconv"
	"strings"

	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
)

// Fingerprint returns the grouping hash of the event: 32 hex characters.
func Fingerprint(event *sentry.Event) string {
	sum := sha256.Sum256([]byte(strings.Join(Components(event), "\n")))
	return hex.EncodeToString(sum[:16])
}

// Components returns the values the fingerprint is built from.
// Exported so tests and, later, the UI can show why events were grouped.
func Components(event *sentry.Event) []string {
	if len(event.Fingerprint) == 0 {
		return defaultComponents(event)
	}

	components := []string{"custom"}

	for _, value := range event.Fingerprint {
		if isDefaultPlaceholder(value) {
			components = append(components, defaultComponents(event)...)
		} else {
			components = append(components, value)
		}
	}

	return components
}

func isDefaultPlaceholder(value string) bool {
	return strings.ReplaceAll(value, " ", "") == "{{default}}"
}

func defaultComponents(event *sentry.Event) []string {
	main := event.MainException()
	// captureMessage sends an exception without a type and with a synthetic stack
	// of the call site. Group it by the message: different messages logged from
	// the same place are different problems.
	isMessage := main == nil || (main.Type == "" && event.Message != "")

	if isMessage {
		text := string(event.Message)

		if text == "" && main != nil {
			text = main.Value
		}

		return []string{"message", NormalizeMessage(text)}
	}

	components := []string{"exception"}

	for _, exception := range event.Exception {
		components = append(components, "type:"+exception.Type)
		frames := groupingFrames(exception.Stacktrace)

		if len(frames) == 0 {
			// No stack: the value is all we have to tell errors of one type apart.
			components = append(components, "value:"+NormalizeMessage(exception.Value))
			continue
		}

		hasSource := false

		for _, frame := range frames {
			components = append(components, "frame:"+frameKey(frame))
			hasSource = hasSource || strings.TrimSpace(frame.ContextLine) != ""
		}

		if !hasSource {
			components = append(components, "value:"+NormalizeMessage(exception.Value))
		}
	}

	return components
}

// groupingFrames returns the in-app frames, or all frames if none is marked in-app.
func groupingFrames(stacktrace *sentry.Stacktrace) []sentry.Frame {
	if stacktrace == nil {
		return nil
	}

	var inApp []sentry.Frame

	for _, frame := range stacktrace.Frames {
		if frame.InApp != nil && *frame.InApp {
			inApp = append(inApp, frame)
		}
	}

	if len(inApp) > 0 {
		return inApp
	}

	return stacktrace.Frames
}

// frameLocation is the module, or the file if there is no module. Both are
// normalized: SDKs build the module from the file path, so it carries the same
// changing parts (e.g. tsx watch adds "?<timestamp>" to the path on every restart).
func frameLocation(frame sentry.Frame) string {
	if frame.Module != "" {
		return normalizeFilename(frame.Module)
	}

	return normalizeFilename(frame.Filename)
}

// maxSourceLineLength keeps minified one-line bundles from bloating the components.
const maxSourceLineLength = 200

func frameKey(frame sentry.Frame) string {
	key := frameLocation(frame)

	if !isAnonymous(frame.Function) {
		key += " " + frame.Function
	}

	// Whitespace-only edits (indentation) must not split a group.
	line := strings.Join(strings.Fields(frame.ContextLine), " ")

	switch {
	case line != "":
		if len(line) > maxSourceLineLength {
			line = line[:maxSourceLineLength]
		}

		return key + " | " + line
	case isAnonymous(frame.Function):
		// No name and no source: the line number is less stable across deploys,
		// but better than merging every anonymous function in the file.
		return key + " line " + strconv.Itoa(frame.Lineno)
	default:
		return key
	}
}

func isAnonymous(function string) bool {
	switch function {
	case "", "?", "<anonymous>", "anonymous", "<unknown>":
		return true
	}

	return false
}

var (
	bundleHashRe = regexp.MustCompile(`[.-][0-9a-fA-F]{8,}\b`)
	uuidRe       = regexp.MustCompile(`[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}`)
	hexRe        = regexp.MustCompile(`\b(0x)?[0-9a-fA-F]{8,}\b`)
	numberRe     = regexp.MustCompile(`\d+`)
)

// normalizeFilename drops what changes between deploys and environments:
// the origin of browser URLs (localhost vs production domain), query strings,
// and content hashes in bundle names (main.3f2a1b9c.js).
func normalizeFilename(filename string) string {
	if index := strings.Index(filename, "://"); index >= 0 {
		rest := filename[index+3:]

		if slash := strings.IndexByte(rest, '/'); slash >= 0 {
			filename = rest[slash:]
		} else {
			filename = "/"
		}
	}

	if index := strings.IndexAny(filename, "?#"); index >= 0 {
		filename = filename[:index]
	}

	return bundleHashRe.ReplaceAllString(filename, "")
}

// NormalizeMessage replaces the parts of a message that differ between
// occurrences of the same problem: UUIDs, hex IDs (8+ chars) and numbers.
func NormalizeMessage(message string) string {
	message = uuidRe.ReplaceAllString(message, "<uuid>")
	message = hexRe.ReplaceAllString(message, "<hex>")

	return numberRe.ReplaceAllString(message, "<n>")
}

// LogFingerprint groups log lines that are the same message with different
// values ("order 17 failed" and "order 42 failed"), so a line repeated in a
// loop can be limited to its newest occurrences. Short: it is stored with
// every log and indexed.
func LogFingerprint(body string) string {
	sum := sha256.Sum256([]byte(NormalizeMessage(strings.TrimSpace(body))))

	return hex.EncodeToString(sum[:8])
}

// Culprit is a short "where did it happen" label for the issue list: the
// transaction (e.g. "GET /api/orders") if set, else the function and file of
// the frame that threw.
func Culprit(event *sentry.Event) string {
	if event.Transaction != "" {
		return event.Transaction
	}

	main := event.MainException()

	if main == nil {
		return ""
	}

	frames := groupingFrames(main.Stacktrace)

	if len(frames) == 0 {
		return ""
	}

	frame := frames[len(frames)-1]
	location := frameLocation(frame)

	if frame.Function == "" {
		return location
	}

	return frame.Function + " (" + location + ")"
}
