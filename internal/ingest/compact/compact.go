// Package compact drops the parts of an SDK event that do not help to find
// or fix an error, before the event is stored. An event is kept for weeks,
// many per issue, so what it carries is paid for many times over.
//
// Dropped:
//   - source lines (context, vars) of library frames, when the stack has
//     frames of the application: the app's own code is where a fix goes,
//     and grouping uses only those frames then;
//   - modules (every installed package and its version), debug_meta (debug
//     files for symbolication, which this server does not do), and the
//     integration and package lists of sdk (its name and version stay);
//   - breadcrumbs beyond the newest maxBreadcrumbs.
//
// Everything else stays as the SDK sent it.
package compact

// maxBreadcrumbs: the last steps before the error tell the story; SDKs keep
// up to 100.
const maxBreadcrumbs = 50

// EventFields drops, in place, what does not help a fix from an event
// decoded with json.Decoder.UseNumber (ingest decodes it once for this and
// package scrub).
func EventFields(event map[string]any) {
	delete(event, "modules")
	delete(event, "debug_meta")

	if sdk, ok := event["sdk"].(map[string]any); ok {
		delete(sdk, "integrations")
		delete(sdk, "packages")
	}

	event["breadcrumbs"] = newestBreadcrumbs(event["breadcrumbs"])

	if event["breadcrumbs"] == nil {
		delete(event, "breadcrumbs")
	}

	// Stacks are in exception.values[].stacktrace, threads.values[].stacktrace
	// and the top-level stacktrace (a message with attachStacktrace).
	for _, key := range []string{"exception", "threads"} {
		for _, item := range values(event[key]) {
			if object, ok := item.(map[string]any); ok {
				compactStacktrace(object["stacktrace"])
			}
		}
	}

	compactStacktrace(event["stacktrace"])
}

// values returns the list of {"values": [...]} or of a bare list.
func values(node any) []any {
	switch typed := node.(type) {
	case []any:
		return typed
	case map[string]any:
		list, _ := typed["values"].([]any)

		return list
	}

	return nil
}

// newestBreadcrumbs keeps the newest maxBreadcrumbs, in the shape they came in.
func newestBreadcrumbs(node any) any {
	trim := func(list []any) []any { return list[max(0, len(list)-maxBreadcrumbs):] }

	switch typed := node.(type) {
	case []any:
		return trim(typed)
	case map[string]any:
		if list, ok := typed["values"].([]any); ok {
			typed["values"] = trim(list)
		}

		return typed
	}

	return node
}

// compactStacktrace drops the source and variables of library frames when
// the stack has frames of the application.
func compactStacktrace(node any) {
	stacktrace, ok := node.(map[string]any)

	if !ok {
		return
	}

	frames, _ := stacktrace["frames"].([]any)
	hasAppFrames := false

	for _, item := range frames {
		if frame, ok := item.(map[string]any); ok && frame["in_app"] == true {
			hasAppFrames = true

			break
		}
	}

	if !hasAppFrames {
		return
	}

	for _, item := range frames {
		if frame, ok := item.(map[string]any); ok && frame["in_app"] != true {
			for _, key := range []string{"pre_context", "context_line", "post_context", "vars"} {
				delete(frame, key)
			}
		}
	}
}
