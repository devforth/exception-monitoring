package notify

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
)

var sample = Notification{
	Kind:        NewIssue,
	Project:     "backend",
	IssueID:     42,
	IssueURL:    "https://errors.example.com/issues/42",
	Title:       "TypeError: Cannot read properties of undefined (reading 'id') <script>",
	Culprit:     "GET /api/orders",
	Level:       "error",
	Environment: "production",
	Time:        time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC),
}

// recordingServer answers every request with status and body and keeps the last request.
func recordingServer(t *testing.T, status int, headers map[string]string, body string) (*httptest.Server, *http.Request, *[]byte) {
	t.Helper()

	var (
		last    http.Request
		payload []byte
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		last = *r
		payload, _ = io.ReadAll(r.Body)

		for name, value := range headers {
			w.Header().Set(name, value)
		}

		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(server.Close)

	return server, &last, &payload
}

func TestSlack(t *testing.T) {
	server, _, payload := recordingServer(t, http.StatusOK, nil, "ok")
	slack := Slack{WebhookURL: server.URL}

	if err := slack.Send(context.Background(), sample); err != nil {
		t.Fatal(err)
	}

	var message struct{ Text string }

	if err := json.Unmarshal(*payload, &message); err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		"*New issue* in `backend` · error",
		"<https://errors.example.com/issues/42|TypeError: Cannot read properties of undefined (reading 'id') &lt;script&gt;>",
		"GET /api/orders · production",
		`Exception Monitoring MCP: get_issue {"issue_id":42}`,
	} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("missing %q in:\n%s", want, message.Text)
		}
	}

	if err := slack.SendSummary(context.Background(), Summary{Count: 12, Window: time.Minute, Items: []Notification{sample}}); err != nil {
		t.Fatal(err)
	}

	_ = json.Unmarshal(*payload, &message)

	if !strings.Contains(message.Text, "12 more issues in the last 1m0s") || !strings.Contains(message.Text, "…and 11 more") || !strings.Contains(message.Text, `get_issue {"issue_id":42}`) {
		t.Errorf("summary:\n%s", message.Text)
	}
}

func TestSlackRateLimit(t *testing.T) {
	server, _, _ := recordingServer(t, http.StatusTooManyRequests, map[string]string{"Retry-After": "7"}, "rate_limited")

	err := Slack{WebhookURL: server.URL}.Send(context.Background(), sample)

	var retryAfter *RetryAfterError

	if !errors.As(err, &retryAfter) || retryAfter.Delay != 7*time.Second {
		t.Fatalf("got %v, want a RetryAfterError of 7s", err)
	}
}

// The webhook URL is the secret: a failed request must not put it into the
// error, which is stored in the outbox, shown and logged.
func TestSlackErrorHidesWebhook(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	webhook := server.URL + "/services/T000/B000/webhook-secret"
	server.Close() // nothing listens: the request fails before an answer

	err := Slack{WebhookURL: webhook}.Send(context.Background(), sample)

	if err == nil || strings.Contains(err.Error(), "webhook-secret") {
		t.Fatalf("error %v, want one without the webhook", err)
	}
}

func TestTelegram(t *testing.T) {
	server, request, payload := recordingServer(t, http.StatusOK, nil, `{"ok":true}`)
	telegram := Telegram{BotToken: "123:SECRET", ChatID: "-100500", apiURL: server.URL}

	if err := telegram.Send(context.Background(), Notification{Kind: Regression, IssueID: 42, Project: "frontend", Title: "a < b", Level: "fatal"}); err != nil {
		t.Fatal(err)
	}

	if request.URL.Path != "/bot123:SECRET/sendMessage" {
		t.Fatalf("path = %s", request.URL.Path)
	}

	var message struct {
		ChatID    string `json:"chat_id"`
		Text      string `json:"text"`
		ParseMode string `json:"parse_mode"`
	}

	if err := json.Unmarshal(*payload, &message); err != nil {
		t.Fatal(err)
	}

	// No PUBLIC_URL: no link. HTML is escaped because parse_mode is HTML.
	if message.ChatID != "-100500" || message.ParseMode != "HTML" ||
		!strings.Contains(message.Text, "<b>Regression</b> in <code>frontend</code> · fatal") || !strings.Contains(message.Text, "<b>a &lt; b</b>") ||
		!strings.Contains(html.UnescapeString(message.Text), `Exception Monitoring MCP: get_issue {"issue_id":42}`) {
		t.Fatalf("message: %+v", message)
	}
}

// An error is one message: the agent prompt as a file, the alert as its caption.
func TestTelegramPromptFile(t *testing.T) {
	type call struct {
		method, chatID, caption, parseMode, fileName, file string
	}

	var (
		calls    []call
		failFile bool
	)

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]

		if method == "sendMessage" {
			var message struct {
				Text string `json:"text"`
			}

			_ = json.NewDecoder(r.Body).Decode(&message)
			calls = append(calls, call{method: method, caption: message.Text})
			_, _ = io.WriteString(w, `{"ok":true}`)

			return
		}

		recorded := call{method: method, chatID: r.FormValue("chat_id"), caption: r.FormValue("caption"), parseMode: r.FormValue("parse_mode")}

		if file, header, err := r.FormFile("document"); err == nil {
			data, _ := io.ReadAll(file)
			recorded.fileName, recorded.file = header.Filename, string(data)
		}

		calls = append(calls, recorded)

		if failFile {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"ok":false,"description":"Bad Request: file is too big"}`)

			return
		}

		_, _ = io.WriteString(w, `{"ok":true}`)
	}))
	t.Cleanup(server.Close)

	telegram := Telegram{BotToken: "123:SECRET", ChatID: "-100500", apiURL: server.URL}
	notification := sample
	notification.Culprit = strings.Repeat("x", 2000)
	notification.Event = &sentry.Event{Exception: sentry.ExceptionList{{Type: "TypeError", Value: "Cannot read properties of undefined"}}}

	if err := telegram.Send(context.Background(), notification); err != nil {
		t.Fatal(err)
	}

	if len(calls) != 1 {
		t.Fatalf("calls: %+v, want one message with the file", calls)
	}

	document := calls[0]

	if document.method != "sendDocument" || document.chatID != "-100500" || document.fileName != "backend-error-42.md" || document.parseMode != "HTML" ||
		!strings.Contains(document.caption, "<b>New issue</b> in <code>backend</code>") ||
		!strings.Contains(html.UnescapeString(document.caption), `Exception Monitoring MCP: get_issue {"issue_id":42}`) || strings.Contains(document.caption, promptCaption) ||
		!strings.Contains(document.file, "Do not change any code") || !strings.Contains(document.file, "Cannot read properties of undefined") {
		t.Errorf("document: %+v", document)
	}

	if length := len([]rune(document.caption)); length > 1024 {
		t.Errorf("caption has %d characters, Telegram takes 1024", length)
	}

	// A refused file does not lose the alert: it goes as a plain message.
	calls, failFile = nil, true

	if err := telegram.Send(context.Background(), notification); err != nil || len(calls) != 2 || calls[1].method != "sendMessage" || !strings.Contains(html.UnescapeString(calls[1].caption), `get_issue {"issue_id":42}`) {
		t.Errorf("refused file: %v, %+v", err, calls)
	}

	// A test message (no event) and a summary are plain messages.
	calls = nil
	_ = telegram.Send(context.Background(), sample)
	_ = telegram.SendSummary(context.Background(), Summary{Count: 2, Window: time.Minute, Items: []Notification{sample}})

	if len(calls) != 2 || calls[0].method != "sendMessage" || calls[1].method != "sendMessage" || !strings.Contains(html.UnescapeString(calls[1].caption), `get_issue {"issue_id":42}`) {
		t.Errorf("without an event: %+v", calls)
	}
}

func TestTelegramErrors(t *testing.T) {
	server, _, _ := recordingServer(t, http.StatusTooManyRequests, nil, `{"ok":false,"description":"Too Many Requests","parameters":{"retry_after":5}}`)

	err := Telegram{BotToken: "123:SECRET", ChatID: "1", apiURL: server.URL}.Send(context.Background(), sample)

	var retryAfter *RetryAfterError

	if !errors.As(err, &retryAfter) || retryAfter.Delay != 5*time.Second {
		t.Fatalf("got %v, want a RetryAfterError of 5s", err)
	}

	// A connection error must not print the URL, which contains the bot token.
	unreachable := httptest.NewServer(http.NotFoundHandler())
	unreachable.Close()

	err = Telegram{BotToken: "123:SECRET", ChatID: "1", apiURL: unreachable.URL}.Send(context.Background(), sample)

	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("error leaks the token or is missing: %v", err)
	}
}

// slackAPI is a fake Slack Web API that keeps every chat.postMessage call.
type slackAPI struct {
	mutex sync.Mutex
	calls []slackCall
}

type slackCall struct {
	Authorization string
	Channel       string `json:"channel"`
	Text          string `json:"text"`
	Thread        string `json:"thread_ts"`
}

func newSlackAPI(t *testing.T, answer string) (*slackAPI, string) {
	t.Helper()

	api := &slackAPI{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var call slackCall

		_ = json.NewDecoder(r.Body).Decode(&call)
		call.Authorization = r.Header.Get("Authorization")

		api.mutex.Lock()
		api.calls = append(api.calls, call)
		api.mutex.Unlock()

		_, _ = io.WriteString(w, answer)
	}))
	t.Cleanup(server.Close)

	return api, server.URL
}

// With a bot the message stays short and the agent prompt goes in a thread
// under it.
func TestSlackBotPromptInThread(t *testing.T) {
	api, url := newSlackAPI(t, `{"ok":true,"ts":"1700000000.000100"}`)
	slack := Slack{BotToken: "xoxb-test-token", Channel: "C0123456789", apiURL: url}
	notification := sample
	notification.Event = &sentry.Event{Exception: sentry.ExceptionList{{Type: "TypeError", Value: "Cannot read properties of undefined"}}}

	if err := slack.Send(context.Background(), notification); err != nil {
		t.Fatal(err)
	}

	if len(api.calls) != 2 {
		t.Fatalf("%d calls, want the message and the prompt", len(api.calls))
	}

	message, prompt := api.calls[0], api.calls[1]

	if message.Authorization != "Bearer xoxb-test-token" || message.Channel != "C0123456789" || message.Thread != "" || strings.Contains(message.Text, "```") {
		t.Errorf("message: %+v", message)
	}

	if prompt.Thread != "1700000000.000100" || !strings.Contains(prompt.Text, "```") || !strings.Contains(prompt.Text, "Do not change any code") {
		t.Errorf("prompt: %+v", prompt)
	}

	// A test message (no event) has no prompt; a summary is one message.
	api.calls = nil
	_ = slack.Send(context.Background(), sample)
	_ = slack.SendSummary(context.Background(), Summary{Count: 2, Window: time.Minute, Items: []Notification{sample}})

	if len(api.calls) != 2 || api.calls[0].Thread != "" || api.calls[1].Thread != "" {
		t.Errorf("without an event: %+v", api.calls)
	}
}

// Slack answers 200 with ok:false: the error says why, and no thread follows.
func TestSlackBotError(t *testing.T) {
	api, url := newSlackAPI(t, `{"ok":false,"error":"not_in_channel"}`)
	slack := Slack{BotToken: "xoxb-test-token", Channel: "C0123456789", apiURL: url}
	notification := sample
	notification.Event = &sentry.Event{}

	err := slack.Send(context.Background(), notification)

	if err == nil || !strings.Contains(err.Error(), "not_in_channel") || strings.Contains(err.Error(), "xoxb") || len(api.calls) != 1 {
		t.Fatalf("err %v, %d calls", err, len(api.calls))
	}
}

// Setup errors say what to fix.
func TestSlackBotErrorHints(t *testing.T) {
	_, url := newSlackAPI(t, `{"ok":false,"error":"missing_scope","needed":"chat:write"}`)
	slack := Slack{BotToken: "xoxb-test-token", Channel: "C0123456789", apiURL: url}

	if err := slack.Send(context.Background(), sample); err == nil || !strings.Contains(err.Error(), "lacks the chat:write scope") || !strings.Contains(err.Error(), "Reinstall to Workspace") {
		t.Fatalf("err %v", err)
	}
}
