// Package compact drops the parts of an SDK event that do not help to find
// or fix an error, before the event is stored.
package compact

import (
	"encoding/json"
	"encoding/json/jsontext"

	"github.com/devforth/exception-monitoring/internal/ingest/scrub"
	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
)

// maxBreadcrumbs: the last steps before the error tell the story; SDKs keep
// up to 100.
const maxBreadcrumbs = 50

// Event writes the next value of decoder, an event, with what does not help
// a fix dropped and credentials removed (package scrub), in one token pass.
// Anything but an object is only scrubbed.
func Event(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
	if decoder.PeekKind() != '{' {
		return scrub.Stream(decoder, encoder)
	}

	return scrub.Object(decoder, encoder, func(name string, decoder *jsontext.Decoder, encoder *jsontext.Encoder) (bool, error) {
		switch name {
		case "modules", "debug_meta":
			return true, decoder.SkipValue()
		case "sdk":
			return true, member(name, decoder, encoder, func(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
				if decoder.PeekKind() != '{' {
					return scrub.Stream(decoder, encoder)
				}

				return scrub.Object(decoder, encoder, func(name string, decoder *jsontext.Decoder, _ *jsontext.Encoder) (bool, error) {
					if name == "integrations" || name == "packages" {
						return true, decoder.SkipValue()
					}

					return false, nil
				})
			})
		case "breadcrumbs":
			if decoder.PeekKind() == 'n' { // nothing to keep: the field goes
				return true, decoder.SkipValue()
			}

			return true, member(name, decoder, encoder, breadcrumbs)
		case "exception", "threads":
			return true, member(name, decoder, encoder, withStacktraces)
		case "stacktrace":
			return true, member(name, decoder, encoder, stacktrace)
		}

		return false, nil
	})
}

// member writes the key of an object member and its value through write.
func member(name string, decoder *jsontext.Decoder, encoder *jsontext.Encoder, write func(*jsontext.Decoder, *jsontext.Encoder) error) error {
	if err := encoder.WriteToken(jsontext.String(name)); err != nil {
		return err
	}

	return write(decoder, encoder)
}

// breadcrumbs keeps the newest maxBreadcrumbs, as a list or {"values": [...]}.
func breadcrumbs(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
	switch decoder.PeekKind() {
	case '[':
		return newest(decoder, encoder)
	case '{':
		return scrub.Object(decoder, encoder, func(name string, decoder *jsontext.Decoder, encoder *jsontext.Encoder) (bool, error) {
			if name != "values" || decoder.PeekKind() != '[' {
				return false, nil
			}

			return true, member(name, decoder, encoder, newest)
		})
	}

	return scrub.Stream(decoder, encoder)
}

// newest writes the last maxBreadcrumbs elements of the next list.
func newest(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
	items, err := elements(decoder)

	if err != nil {
		return err
	}

	return writeList(encoder, items[max(0, len(items)-maxBreadcrumbs):], func(_ int, decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
		return scrub.Stream(decoder, encoder)
	})
}

// withStacktraces writes exception or threads: a list of items, or
// {"values": [...]}, each item's stacktrace compacted.
func withStacktraces(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
	items := func(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
		return scrub.Values(decoder, encoder, func(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
			if decoder.PeekKind() != '{' {
				return scrub.Stream(decoder, encoder)
			}

			return scrub.Object(decoder, encoder, func(name string, decoder *jsontext.Decoder, encoder *jsontext.Encoder) (bool, error) {
				if name != "stacktrace" {
					return false, nil
				}

				return true, member(name, decoder, encoder, stacktrace)
			})
		})
	}

	switch decoder.PeekKind() {
	case '[':
		return items(decoder, encoder)
	case '{':
		return scrub.Object(decoder, encoder, func(name string, decoder *jsontext.Decoder, encoder *jsontext.Encoder) (bool, error) {
			if name != "values" || decoder.PeekKind() != '[' {
				return false, nil
			}

			return true, member(name, decoder, encoder, items)
		})
	}

	return scrub.Stream(decoder, encoder)
}

// stacktrace drops the source and variables of library frames when the
// stack has frames of the application; the frames are read first to know.
func stacktrace(decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
	if decoder.PeekKind() != '{' {
		return scrub.Stream(decoder, encoder)
	}

	return scrub.Object(decoder, encoder, func(name string, decoder *jsontext.Decoder, encoder *jsontext.Encoder) (bool, error) {
		if name != "frames" || decoder.PeekKind() != '[' {
			return false, nil
		}

		frames, err := elements(decoder)

		if err != nil {
			return true, err
		}

		app := make([]bool, len(frames))
		hasAppFrames := false

		for index, frame := range frames {
			app[index] = isAppFrame(frame)
			hasAppFrames = hasAppFrames || app[index]
		}

		return true, member(name, decoder, encoder, func(_ *jsontext.Decoder, encoder *jsontext.Encoder) error {
			return writeList(encoder, frames, func(index int, decoder *jsontext.Decoder, encoder *jsontext.Encoder) error {
				if !hasAppFrames || app[index] || decoder.PeekKind() != '{' {
					return scrub.Stream(decoder, encoder)
				}

				return scrub.Object(decoder, encoder, func(name string, decoder *jsontext.Decoder, _ *jsontext.Encoder) (bool, error) {
					if librarySource[name] {
						return true, decoder.SkipValue()
					}

					return false, nil
				})
			})
		})
	})
}

// librarySource are the fields of a library frame dropped when the stack has frames of the application.
var librarySource = map[string]bool{"pre_context": true, "context_line": true, "post_context": true, "vars": true}

// elements reads the next list as its raw elements.
func elements(decoder *jsontext.Decoder) ([]jsontext.Value, error) {
	if _, err := decoder.ReadToken(); err != nil {
		return nil, err
	}

	var items []jsontext.Value

	for decoder.PeekKind() != ']' {
		item, err := decoder.ReadValue()

		if err != nil {
			return nil, err
		}

		items = append(items, item.Clone()) // a read value is valid only until the next read
	}

	_, err := decoder.ReadToken()

	return items, err
}

// writeList writes raw elements as a list, each through write.
func writeList(encoder *jsontext.Encoder, items []jsontext.Value, write func(index int, decoder *jsontext.Decoder, encoder *jsontext.Encoder) error) error {
	if err := encoder.WriteToken(jsontext.BeginArray); err != nil {
		return err
	}

	for index, item := range items {
		if err := write(index, scrub.NewDecoder(item), encoder); err != nil {
			return err
		}
	}

	return encoder.WriteToken(jsontext.EndArray)
}

// isAppFrame is sentry.Frame.IsInApp on a raw frame: marked in-app by the
// SDK, and not a file of an installed package.
func isAppFrame(raw jsontext.Value) bool {
	var frame struct {
		InApp    any `json:"in_app"`
		Filename any `json:"filename"`
		AbsPath  any `json:"abs_path"`
	}

	if raw.Kind() != '{' || json.Unmarshal(raw, &frame) != nil {
		return false
	}

	filename, _ := frame.Filename.(string)
	absPath, _ := frame.AbsPath.(string)

	return frame.InApp == true && !sentry.IsLibraryPath(filename) && !sentry.IsLibraryPath(absPath)
}
