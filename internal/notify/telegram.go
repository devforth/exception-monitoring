package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strings"
	"time"

	textutil "github.com/devforth/exception-monitoring/internal/platform/text"
)

// Telegram sends messages with a bot (https://core.telegram.org/bots/api#sendmessage).
// The bot must be a member of the chat; ChatID is the numeric ID of a user,
// group (negative) or channel.
type Telegram struct {
	BotToken string
	ChatID   string
	AppName  string // APP_NAME, in the MCP request of an issue
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
		"<b>" + telegramLink(notification.IssueURL, textutil.Shorten(notification.Title, titleLength)) + "</b>",
	}

	// Capped so the text fits a file caption (1024 characters).
	if details := notification.details(); details != "" {
		lines = append(lines, html.EscapeString(textutil.Shorten(details, detailsLength)))
	}

	if reference := mcpReference(telegram.AppName, notification); reference != "" {
		lines = append(lines, "<code>"+html.EscapeString(reference)+"</code>")
	}

	if notification.Event == nil {
		return telegram.send(ctx, strings.Join(lines, "\n"))
	}

	// One message: the prompt for a coding agent as a file, the alert as its
	// caption. A message holds at most 4096 characters and a copy button
	// 256, too few for a prompt.
	// The file itself is the prompt (its name ends in .md), so the caption is
	// just the alert.
	err := telegram.sendDocument(ctx, promptFileName(notification), []byte(agentPrompt(notification)), strings.Join(lines, "\n"))

	var retryAfter *RetryAfterError

	if err == nil || errors.As(err, &retryAfter) {
		return err
	}

	// The file was refused: the alert still goes, without it.
	return telegram.send(ctx, strings.Join(lines, "\n"))
}

func (telegram Telegram) SendSummary(ctx context.Context, summary Summary) error {
	lines := []string{"⚠️ <b>" + html.EscapeString(summary.heading()) + "</b>"}

	for _, item := range summary.Items {
		lines = append(lines, fmt.Sprintf("• <code>%s</code> %s", html.EscapeString(item.Project), telegramLink(item.IssueURL, textutil.Shorten(item.Title, shortTitleLength))))

		if reference := mcpReference(telegram.AppName, item); reference != "" {
			lines = append(lines, "<code>"+html.EscapeString(reference)+"</code>")
		}
	}

	lines = append(lines, "<i>"+html.EscapeString(summary.note())+"</i>")

	return telegram.send(ctx, strings.Join(lines, "\n"))
}

func (telegram Telegram) send(ctx context.Context, text string) error {
	return telegram.call(ctx, "sendMessage", map[string]any{
		"chat_id":                  telegram.ChatID,
		"text":                     text,
		"parse_mode":               "HTML",
		"disable_web_page_preview": true,
	}, nil)
}

// sendDocument sends a file with an HTML caption
// (https://core.telegram.org/bots/api#senddocument).
func (telegram Telegram) sendDocument(ctx context.Context, name string, data []byte, caption string) error {
	var body bytes.Buffer

	form := multipart.NewWriter(&body)
	fields := map[string]string{"chat_id": telegram.ChatID, "caption": caption, "parse_mode": "HTML"}

	for field, value := range fields {
		if err := form.WriteField(field, value); err != nil {
			return err
		}
	}

	file, err := form.CreateFormFile("document", name)

	if err != nil {
		return err
	}

	if _, err := file.Write(data); err != nil {
		return err
	}

	if err := form.Close(); err != nil {
		return err
	}

	return telegram.post(ctx, "sendDocument", form.FormDataContentType(), &body, nil)
}

// ChatTitle asks Telegram for the name of the chat
// (https://core.telegram.org/bots/api#getchat): the title of a group or
// channel, or the name of a person.
func (telegram Telegram) ChatTitle(ctx context.Context) (string, error) {
	var chat struct {
		Title     string `json:"title"`
		FirstName string `json:"first_name"`
		LastName  string `json:"last_name"`
		Username  string `json:"username"`
	}

	if err := telegram.call(ctx, "getChat", map[string]any{"chat_id": telegram.ChatID}, &chat); err != nil {
		return "", err
	}

	if chat.Title != "" {
		return chat.Title, nil
	}

	if name := strings.TrimSpace(chat.FirstName + " " + chat.LastName); name != "" {
		return name, nil
	}

	return chat.Username, nil
}

// call runs a Bot API method; the "result" of the answer goes into result
// when it is not nil.
func (telegram Telegram) call(ctx context.Context, method string, payload, result any) error {
	body, err := json.Marshal(payload)

	if err != nil {
		return err
	}

	return telegram.post(ctx, method, "application/json", bytes.NewReader(body), result)
}

// post sends a request body of contentType to a Bot API method.
func (telegram Telegram) post(ctx context.Context, method, contentType string, body io.Reader, result any) error {
	apiURL := telegram.apiURL

	if apiURL == "" {
		apiURL = "https://api.telegram.org"
	}

	request, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL+"/bot"+telegram.BotToken+"/"+method, body)

	if err != nil {
		return err
	}

	request.Header.Set("Content-Type", contentType)

	response, err := http.DefaultClient.Do(request)

	if err != nil {
		// The URL contains the bot token: never let it reach the logs.
		return fmt.Errorf("telegram: %w", stripURL(err))
	}

	defer func() { _ = response.Body.Close() }()

	// Errors come as {"ok": false, "description": ..., "parameters": {"retry_after": N}}.
	var answer struct {
		OK          bool            `json:"ok"`
		Description string          `json:"description"`
		Result      json.RawMessage `json:"result"`
		Parameters  struct {
			RetryAfter int `json:"retry_after"`
		} `json:"parameters"`
	}

	data, _ := io.ReadAll(io.LimitReader(response.Body, 64<<10))
	_ = json.Unmarshal(data, &answer)

	if response.StatusCode == http.StatusOK && answer.OK {
		if result != nil {
			return json.Unmarshal(answer.Result, result)
		}

		return nil
	}

	err = fmt.Errorf("telegram: %d %s", response.StatusCode, answer.Description)

	if response.StatusCode == http.StatusTooManyRequests {
		return &RetryAfterError{Delay: time.Duration(answer.Parameters.RetryAfter) * time.Second, Err: err}
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
// as *url.Error, whose message includes the URL, and so the Telegram bot
// token or the Slack webhook.
func stripURL(err error) error {
	var urlErr *url.Error

	if errors.As(err, &urlErr) {
		return urlErr.Err
	}

	return err
}
