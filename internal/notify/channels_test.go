package notify

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
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
	} {
		if !strings.Contains(message.Text, want) {
			t.Errorf("missing %q in:\n%s", want, message.Text)
		}
	}

	if err := slack.SendSummary(context.Background(), Summary{Count: 12, Window: time.Minute, Items: []Notification{sample}}); err != nil {
		t.Fatal(err)
	}

	_ = json.Unmarshal(*payload, &message)

	if !strings.Contains(message.Text, "12 more issues in the last 1m0s") || !strings.Contains(message.Text, "…and 11 more") {
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

func TestTelegram(t *testing.T) {
	server, request, payload := recordingServer(t, http.StatusOK, nil, `{"ok":true}`)
	telegram := Telegram{BotToken: "123:SECRET", ChatID: "-100500", apiURL: server.URL}

	if err := telegram.Send(context.Background(), Notification{Kind: Regression, Project: "frontend", Title: "a < b", Level: "fatal"}); err != nil {
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
		!strings.Contains(message.Text, "<b>Regression</b> in <code>frontend</code> · fatal") || !strings.Contains(message.Text, "<b>a &lt; b</b>") {
		t.Fatalf("message: %+v", message)
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
