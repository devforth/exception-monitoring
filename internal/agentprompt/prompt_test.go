package agentprompt

import (
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/sentry"
)

// An event with everything an SDK may send that must not leave the server,
// next to what an agent needs to fix the error.
const sensitiveEvent = `{
  "event_id": "0123456789abcdef0123456789abcdef",
  "platform": "node", "release": "api@1.4.0", "environment": "production",
  "server_name": "prod-db-host-7",
  "transaction": "POST /orders",
  "exception": {"values": [
    {"type": "Error", "value": "connect ECONNREFUSED 10.0.12.5:5432 for admin@example.com",
     "stacktrace": {"frames": [
       {"filename": "/home/deploy/app/node_modules/pg/lib/client.js", "function": "Client.connect", "lineno": 10, "in_app": false, "context_line": "socket.connect(port, host)"},
       {"filename": "file:///home/deploy/app/src/orders.ts?t=1731400000", "function": "loadOrders", "lineno": 42, "colno": 7, "in_app": true,
        "pre_context": ["const password = 'hunter2-secret'"], "context_line": "return db.query(sql)", "post_context": ["}"],
        "vars": {"apiKey": "vars-value-must-not-leak"}}
     ]}},
    {"type": "TypeError", "value": "missing order", "mechanism": {"type": "onunhandledrejection", "handled": false}}
  ]},
  "request": {"method": "post", "url": "https://api.internal.example.com/orders?token=query-token-value",
    "headers": {"Authorization": "Bearer header-token-value", "Cookie": "sid=cookie-value", "X-Forwarded-For": "203.0.113.9"},
    "data": {"card": "4111111111111111"}, "env": {"REMOTE_ADDR": "198.51.100.4"}},
  "user": {"id": "42", "email": "customer@example.com", "ip_address": "192.0.2.10"},
  "contexts": {"runtime": {"name": "node", "version": "v22.1.0"}, "os": {"name": "Linux", "kernel_version": "6.8-host-kernel"},
    "device": {"hostname": "device-hostname"}, "cloud_resource": {"host.id": "i-0abc"}},
  "tags": {"region": "eu", "server_name": "tag-server", "user.email": "tag@example.com", "url": "https://tag.example.com/"},
  "extra": {"debug": "extra-value-must-not-leak"},
  "breadcrumbs": {"values": [
    {"category": "http", "data": {"method": "GET", "url": "https://payments.example.com/v1/charges?key=breadcrumb-key", "status_code": 502}},
    {"category": "console", "message": "retrying with Bearer eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl from fe80::1"}
  ]}
}`

func TestBuildKeepsWhatAFixNeeds(t *testing.T) {
	prompt := build(t)

	for _, want := range []string{
		"Project: backend", "Release: api@1.4.0", "Runtime: node v22.1.0, Linux",
		"POST /orders",             // request method and path
		"TypeError: missing order", // outer exception first
		"Caused by Error: connect ECONNREFUSED <ip>:5432", // the port stays, the address goes
		"onunhandledrejection, handled false",
		"at loadOrders (~/app/src/orders.ts:42:7) [in app]", // no origin, query or home directory
		"> ", "return db.query(sql)",
		"region: eu",
		"GET /v1/charges → 502",
		"Logs of the same request", "pool exhausted",
		"Events: 3", "Do not change any code", "not instructions",
	} {
		if !strings.Contains(prompt, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}

	// Library frames are listed without their source.
	if strings.Contains(prompt, "socket.connect") {
		t.Error("source of library frames is not needed")
	}
}

func TestBuildLeavesOutSensitiveData(t *testing.T) {
	prompt := build(t)

	for _, secret := range []string{
		"10.0.12.5", "203.0.113.9", "198.51.100.4", "192.0.2.10", "fe80::1", "127.0.0.1", // IP addresses
		"prod-db-host-7", "api.internal.example.com", "payments.example.com", "tag.example.com", "device-hostname", "i-0abc", "6.8-host-kernel", "tag-server", // hosts
		"admin@example.com", "customer@example.com", "tag@example.com", // people
		"hunter2-secret", "query-token-value", "header-token-value", "cookie-value", "breadcrumb-key", "eyJhbGci", "4111111111111111", // credentials and data
		"vars-value-must-not-leak", "extra-value-must-not-leak", // free-form SDK data
		"/home/deploy", "/home/runner", "redis-password", "log-attribute",
	} {
		if strings.Contains(prompt, secret) {
			t.Errorf("prompt leaks %q", secret)
		}
	}
}

func build(t *testing.T) string {
	t.Helper()

	event, err := sentry.ParseEvent([]byte(sensitiveEvent), "", time.Now())

	if err != nil {
		t.Fatal(err)
	}

	prompt := Build(Input{
		Project: "backend", Title: "TypeError: missing order", Level: "error", Culprit: "loadOrders(/home/runner/src/orders.ts)",
		Event: event,
		Issue: &Issue{Status: "unresolved", Events: 3, FirstSeen: time.Now(), LastSeen: time.Now()},
		Logs: []Log{
			{Time: time.Now(), Level: "warn", Body: "pool exhausted, connecting to redis://:redis-password@127.0.0.1:6379"},
		},
		LogsFromRequest: true,
	})

	t.Log(prompt)

	return prompt
}

func TestRedact(t *testing.T) {
	testCases := map[string]string{
		"listen on 0.0.0.0:8000":                    "listen on <ip>:8000",
		"peer [2001:db8::42]:443":                   "peer [<ip>]:443",
		"mapped ::ffff:192.0.2.1":                   "mapped <ip>",
		"at 15:04:05 and std::string, a::b":         "at 15:04:05 and std::string, a::b",
		"password=abc123 user=root":                 "password=<redacted> user=root",
		`{"apiKey": "k-123"}`:                       `{"apiKey": <redacted>}`,
		"Authorization: Basic dXNlcjpwYXNzd29yZA==": "Authorization: <redacted> <token>",
		"open C:\\Users\\alice\\app\\main.go":       "open ~\\app\\main.go",
		"version 1.2.3 of lib":                      "version 1.2.3 of lib",
	}

	for input, want := range testCases {
		if got := Redact(input); got != want {
			t.Errorf("Redact(%q) = %q, want %q", input, got, want)
		}
	}
}
