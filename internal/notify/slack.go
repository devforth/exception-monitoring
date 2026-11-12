package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Slack posts to an incoming webhook
// (https://api.slack.com/messaging/webhooks). Mattermost and Rocket.Chat
// accept the same {"text": ...} payload with the same link syntax.
type Slack struct {
	WebhookURL string
	Client     *http.Client
}

func (slack Slack) Name() string { return "slack" }

func (slack Slack) Send(ctx context.Context, notification Notification) error {
	icon := ":rotating_light:"

	if notification.Kind == Regression {
		icon = ":repeat:"
	}

	lines := []string{
		fmt.Sprintf("%s *%s* in `%s` · %s", icon, notification.Kind.label(), slackEscape(notification.Project), slackEscape(notification.Level)),
		"*" + slackLink(notification.IssueURL, truncate(notification.Title, 300)) + "*",
	}

	if details := notification.details(); details != "" {
		lines = append(lines, slackEscape(details))
	}

	return slack.post(ctx, strings.Join(lines, "\n"))
}

func (slack Slack) SendSummary(ctx context.Context, summary Summary) error {
	lines := []string{":warning: *" + summary.heading() + "*"}

	for _, item := range summary.Items {
		lines = append(lines, fmt.Sprintf("• `%s` %s", slackEscape(item.Project), slackLink(item.IssueURL, truncate(item.Title, 150))))
	}

	lines = append(lines, "_"+summary.note()+"_")

	return slack.post(ctx, strings.Join(lines, "\n"))
}

func (slack Slack) post(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]string{"text": text})

	if err != nil {
		return err
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, slack.WebhookURL, bytes.NewReader(body))

	if err != nil {
		return err
	}

	request.Header.Set("Content-Type", "application/json")

	return doRequest(slack.Client, request)
}

// slackEscape escapes the three characters Slack treats as markup in text.
func slackEscape(text string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(text)
}

func slackLink(link, text string) string {
	if link == "" {
		return slackEscape(text)
	}

	return "<" + link + "|" + slackEscape(strings.ReplaceAll(text, "|", "¦")) + ">"
}

// doRequest sends an HTTP request and turns a non-2xx answer into an error;
// 429 becomes a RetryAfterError with the delay from Retry-After.
func doRequest(client *http.Client, request *http.Request) error {
	if client == nil {
		client = http.DefaultClient
	}

	response, err := client.Do(request)

	if err != nil {
		return err
	}

	defer func() { _ = response.Body.Close() }()

	body, _ := io.ReadAll(io.LimitReader(response.Body, 1024))

	if response.StatusCode >= 200 && response.StatusCode < 300 {
		return nil
	}

	err = fmt.Errorf("%s: %d %s", request.URL.Host, response.StatusCode, strings.TrimSpace(string(body)))

	if response.StatusCode == http.StatusTooManyRequests {
		seconds, _ := strconv.Atoi(response.Header.Get("Retry-After"))

		return &RetryAfterError{Delay: time.Duration(seconds) * time.Second, Err: err}
	}

	return err
}
