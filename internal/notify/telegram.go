package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Telegram sends messages with a bot (https://core.telegram.org/bots/api#sendmessage).
// The bot must be a member of the chat; ChatID is the numeric ID of a user,
// group (negative) or channel.
type Telegram struct {
	BotToken string
	ChatID   string
	Client   *http.Client
	// apiURL is replaced in tests with a fake server.
	apiURL string
}

func (telegram Telegram) Name() string { return "telegram" }

func (telegram Telegram) Send(ctx context.Context, notification Notification) error {
	icon := "🚨"

	if notification.Kind == Regression {
		icon = "🔁"
	}

	lines := []string{
		fmt.Sprintf("%s <b>%s</b> in <code>%s</code> · %s", icon, notification.Kind.label(), html.EscapeString(notification.Project), html.EscapeString(notification.Level)),
		"<b>" + telegramLink(notification.IssueURL, truncate(notification.Title, 300)) + "</b>",
	}

	if details := notification.details(); details != "" {
		lines = append(lines, html.EscapeString(details))
	}

	return telegram.send(ctx, strings.Join(lines, "\n"))
}

func (telegram Telegram) SendSummary(ctx context.Context, summary Summary) error {
	lines := []string{"⚠️ <b>" + html.EscapeString(summary.heading()) + "</b>"}

	for _, item := range summary.Items {
		lines = append(lines, fmt.Sprintf("• <code>%s</code> %s", html.EscapeString(item.Project), telegramLink(item.IssueURL, truncate(item.Title, 150))))
	}

	lines = append(lines, "<i>"+html.EscapeString(summary.note())+"</i>")

	return telegram.send(ctx, strings.Join(lines, "\n"))
}

func (telegram Telegram) send(ctx context.Context, text string) error {
	body, err := json.Marshal(map[string]any{
		"chat_id":                  telegram.ChatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	})

	if err != nil {
		return err
	}

	apiURL := telegram.apiURL

	if apiURL == "" {
		apiURL = "https://api.telegram.org"
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/bot"+telegram.BotToken+"/sendMessage", bytes.NewReader(body))

	if err != nil {
		return err
	}

	request.Header.Set("Content-Type", "application/json")

	client := telegram.Client

	if client == nil {
		client = http.DefaultClient
	}

	response, err := client.Do(request)

	if err != nil {
		// The URL contains the bot token: never let it reach the logs.
		return fmt.Errorf("telegram: %w", stripURL(err))
	}

	defer func() { _ = response.Body.Close() }()

	// Errors come as {"ok": false, "description": ..., "parameters": {"retry_after": N}}.
	var result struct {
		OK          bool   `json:"ok"`
		Description string `json:"description"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}

	data, _ := io.ReadAll(io.LimitReader(response.Body, 4096))
	_ = json.Unmarshal(data, &result)

	if response.StatusCode == http.StatusOK && result.OK {
		return nil
	}

	err = fmt.Errorf("telegram: %d %s", response.StatusCode, result.Description)

	if response.StatusCode == http.StatusTooManyRequests {
		return &RetryAfterError{Delay: time.Duration(result.Parameters.RetryAfter) * time.Second, Err: err}
	}

	return err
}

func telegramLink(link, text string) string {
	if link == "" {
		return html.EscapeString(text)
	}

	return `<a href="` + html.EscapeString(link) + `">` + html.EscapeString(text) + "</a>"
}

// stripURL drops the request URL from a transport error: net/http wraps it
// as *url.Error, whose message includes the URL and so the bot token.
func stripURL(err error) error {
	var urlErr *url.Error

	if errors.As(err, &urlErr) {
		return urlErr.Err
	}

	return err
}
