package compact

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestEventDropsWhatDoesNotHelpAFix(t *testing.T) {
	var breadcrumbs []string

	for index := range 80 {
		breadcrumbs = append(breadcrumbs, fmt.Sprintf(`{"message":"step %d"}`, index))
	}

	payload := `{
	  "event_id": "0123456789abcdef0123456789abcdef", "timestamp": 1731400000.123456789,
	  "modules": {"express": "4.19.2", "lodash": "4.17.21"},
	  "debug_meta": {"images": [{"type": "sourcemap", "code_file": "app.js"}]},
	  "sdk": {"name": "sentry.javascript.node", "version": "11.11.0", "integrations": ["Http", "Console"], "packages": [{"name": "npm:@sentry/node"}]},
	  "breadcrumbs": {"values": [` + strings.Join(breadcrumbs, ",") + `]},
	  "exception": {"values": [{"type": "Error", "value": "boom", "stacktrace": {"frames": [
	    {"filename": "node_modules/express/lib/router.js", "function": "handle", "in_app": false,
	     "pre_context": ["library line before"], "context_line": "library line", "post_context": ["library line after"], "vars": {"req": "library vars"}},
	    {"filename": "/app/src/orders.ts", "function": "loadOrders", "in_app": true,
	     "pre_context": ["app line before"], "context_line": "return db.query(sql)", "post_context": ["app line after"], "vars": {"sql": "app vars"}}
	  ]}}]},
	  "request": {"url": "https://example.com/orders"}
	}`

	compacted := string(Event([]byte(payload)))

	for _, gone := range []string{`"modules"`, "lodash", "4.19.2", "debug_meta", "integrations", "npm:@sentry/node", "library line", "library vars", `"step 29"`} {
		if strings.Contains(compacted, gone) {
			t.Errorf("%q must be dropped:\n%s", gone, compacted)
		}
	}

	for _, kept := range []string{
		"app line before", "return db.query(sql)", "app line after", "app vars", // the application's code
		`"name":"sentry.javascript.node"`, `"version":"11.11.0"`, // the SDK itself
		`"step 30"`, `"step 79"`, // the newest breadcrumbs
		`"function":"handle"`, // library frames stay, without their source
		"https://example.com/orders", "1731400000.123456789",
	} {
		if !strings.Contains(compacted, kept) {
			t.Errorf("%q must be kept:\n%s", kept, compacted)
		}
	}

	if !json.Valid([]byte(compacted)) {
		t.Fatal("not valid JSON")
	}
}

// Without frames of the application (some SDKs mark none), every frame keeps
// its source: it is all there is, and grouping uses it.
func TestEventKeepsLibrarySourceWithoutAppFrames(t *testing.T) {
	payload := `{"exception": {"values": [{"type": "Error", "stacktrace": {"frames": [{"filename": "a.js", "context_line": "only source"}]}}]}}`

	if compacted := string(Event([]byte(payload))); !strings.Contains(compacted, "only source") {
		t.Fatalf("source dropped: %s", compacted)
	}
}

func TestEventLeavesOtherInputAlone(t *testing.T) {
	for _, payload := range []string{"not json", `["a list"]`, "null"} {
		if got := string(Event([]byte(payload))); got != payload {
			t.Errorf("Event(%q) = %q", payload, got)
		}
	}
}
