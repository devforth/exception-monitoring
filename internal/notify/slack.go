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

	textutil "github.com/devforth/exception-monitoring/internal/platform/text"
)

// Slack posts to an incoming webhook
// (https://api.slack.com/messaging/webhooks; Mattermost and Rocket.Chat
// accept the same {"text": ...} payload with the same link syntax), or, with
// a bot token, through chat.postMessage, which also lets the agent prompt go
// in a thread under the message: the message itself stays short.
type Slack struct {
	WebhookURL string
	BotToken   string
	Channel    string
	AppName    string // APP_NAME, in the MCP request of an issue
	// apiURL is replaced in tests with a fake server.
	apiURL string
}

// maxSlackText keeps a message under Slack's 40,000-character limit.
const maxSlackText = 39000

func (slack Slack) Name() string { return "slack" }

func (slack Slack) Send(ctx context.Context, notification Notification) error {
	icon := ":rotating_light:"

	if notification.Kind == Regression {
		icon = ":repeat:"
	}

	lines := []string{
		fmt.Sprintf("%s *%s* in `%s` · %s", icon, notification.Kind.label(), slackEscape(notification.Project), slackEscape(notification.Level)),
		"*" + slackLink(notification.IssueURL, textutil.Shorten(notification.Title, titleLength)) + "*",
	}

	if details := notification.details(); details != "" {
		lines = append(lines, slackEscape(details))
	}

	if reference := mcpReference(slack.AppName, notification); reference != "" {
		lines = append(lines, "`"+reference+"`")
	}

	if slack.BotToken == "" {
		return slack.post(ctx, strings.Join(lines, "\n"))
	}

	timestamp, err := slack.postMessage(ctx, strings.Join(lines, "\n"), "")

	if err != nil || notification.Event == nil {
		return err
	}

	// The message is delivered; a failed prompt must not make the outbox
	// send it again (a duplicate in the channel), so its error is dropped.
	_, _ = slack.postMessage(ctx, slackPrompt(notification), timestamp)

	return nil
}

// slackPrompt is the agent prompt as a code block for the thread.
func slackPrompt(notification Notification) string {
	prompt := agentPrompt(notification)
	// A code block ends at the next three backticks: keep them out of it.
	prompt = strings.ReplaceAll(prompt, "```", "ʼʼʼ")
	header := promptCaption + "\n"

	return header + "```" + textutil.Shorten(slackEscape(prompt), maxSlackText-len(header)-6) + "```"
}

func (slack Slack) SendSummary(ctx context.Context, summary Summary) error {
	lines := []string{":warning: *" + summary.heading() + "*"}

	for _, item := range summary.Items {
		lines = append(lines, fmt.Sprintf("• `%s` %s", slackEscape(item.Project), slackLink(item.IssueURL, textutil.Shorten(item.Title, shortTitleLength))))

		if reference := mcpReference(slack.AppName, item); reference != "" {
			lines = append(lines, "  `"+reference+"`")
		}
	}

	lines = append(lines, "_"+summary.note()+"_")

	if slack.BotToken != "" {
		_, err := slack.postMessage(ctx, strings.Join(lines, "\n"), "")

		return err
	}

	return slack.post(ctx, strings.Join(lines, "\n"))
}

// postMessage posts as the bot (https://api.slack.com/methods/chat.postMessage),
// in the thread of threadTimestamp when given, and returns the timestamp of
// the new message. Slack answers 200 with {"ok": false, "error": ...} on
// failures such as not_in_channel; the token travels in a header, so errors
// never contain it.
func (slack Slack) postMessage(ctx context.Context, text, threadTimestamp string) (string, error) {
	payload := map[string]any{"channel": slack.Channel, "text": text, "unfurl_links": false, "unfurl_media": false}

	if threadTimestamp != "" {
		payload["thread_ts"] = threadTimestamp
	}

	body, err := json.Marshal(payload)

	if err != nil {
		return "", err
	}

	apiURL := slack.apiURL

	if apiURL == "" {
		apiURL = "https://slack.com/api"
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/chat.postMessage", bytes.NewReader(body))

	if err != nil {
		return "", err
	}

	request.Header.Set("Content-Type", "application/json; charset=utf-8")
	request.Header.Set("Authorization", "Bearer "+slack.BotToken)

	response, err := http.DefaultClient.Do(request)

	if err != nil {
		return "", fmt.Errorf("slack: %w", stripURL(err))
	}

	defer func() { _ = response.Body.Close() }()

	var answer struct {
		OK        bool   `json:"ok"`
		Error     string `json:"error"`
		Needed    string `json:"needed"` // the missing scope of missing_scope
		Timestamp string `json:"ts"`
	}

	data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	_ = json.Unmarshal(data, &answer)

	if response.StatusCode == http.StatusTooManyRequests {
		seconds, _ := strconv.Atoi(response.Header.Get("Retry-After"))

		return "", &RetryAfterError{Delay: time.Duration(seconds) * time.Second, Err: fmt.Errorf("slack: rate limited")}
	}

	if response.StatusCode != http.StatusOK || !answer.OK {
		return "", fmt.Errorf("slack: %d %s%s", response.StatusCode, answer.Error, slackErrorHint(answer.Error, answer.Needed))
	}

	return answer.Timestamp, nil
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

	return doRequest(request)
}

// slackErrorHint says what to do about the errors a bot setup runs into,
// so the delivery log tells it without a trip to the Slack docs.
func slackErrorHint(code, needed string) string {
	switch code {
	case "missing_scope":
		return fmt.Sprintf(" (the bot token lacks the %s scope: add it under OAuth & Permissions → Bot Token Scopes, then Reinstall to Workspace)", needed)
	case "not_in_channel":
		return " (invite the bot to the channel: /invite @app)"
	case "channel_not_found":
		return " (check the channel ID; a private channel needs the bot invited)"
	case "invalid_auth", "not_authed", "account_inactive", "token_revoked":
		return " (the bot token is not valid: copy the Bot User OAuth Token again)"
	}

	return ""
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
func doRequest(request *http.Request) error {
	response, err := http.DefaultClient.Do(request)

	// The URL of a webhook is its secret: errors are stored, shown and logged.
	if err != nil {
		return stripURL(err)
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
