package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/secret"
	"github.com/devforth/exception-monitoring/internal/store"
)

// Secrets of chat connections, stored as encrypted JSON in Connection.Secret.
type TelegramSecret struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

type SlackSecret struct {
	WebhookURL string `json:"webhook_url"`
}

// Factory turns stored connections into channels: it holds what they share
// (the SMTP server, the encryption key).
type Factory struct {
	SMTP config.SMTP
	Box  *secret.Box // nil without SECRET_KEY: chat connections cannot be used
	// HTTPClient is used by chat channels; nil means http.DefaultClient.
	HTTPClient *http.Client
	// telegramURL replaces the Telegram API in tests.
	telegramURL string
}

// ErrNoSecretKey means a chat connection exists but SECRET_KEY is not set.
var ErrNoSecretKey = errors.New("SECRET_KEY is not set: chat tokens cannot be decrypted")

// EncryptSecret encrypts a TelegramSecret or SlackSecret for storage.
func (factory Factory) EncryptSecret(value any) (string, error) {
	if factory.Box == nil {
		return "", ErrNoSecretKey
	}

	plaintext, err := json.Marshal(value)

	if err != nil {
		return "", err
	}

	return factory.Box.Encrypt(plaintext)
}

// DecryptSecret reads a stored secret into target.
func (factory Factory) DecryptSecret(encrypted string, target any) error {
	if factory.Box == nil {
		return ErrNoSecretKey
	}

	plaintext, err := factory.Box.Decrypt(encrypted)

	if err != nil {
		return err
	}

	return json.Unmarshal(plaintext, target)
}

// Channel builds the channel that delivers to a connection.
func (factory Factory) Channel(connection store.Connection) (Channel, error) {
	switch connection.Type {
	case config.ChannelEmail:
		if !factory.SMTP.Enabled() {
			return nil, errors.New("SMTP is not configured")
		}

		return Email{SMTP: factory.SMTP, To: connection.Address, HideIssueLink: connection.HideIssueLink}, nil
	case config.ChannelTelegram:
		var tokens TelegramSecret

		if err := factory.DecryptSecret(connection.Secret, &tokens); err != nil {
			return nil, err
		}

		return Telegram{BotToken: tokens.BotToken, ChatID: tokens.ChatID, Client: factory.HTTPClient, apiURL: factory.telegramURL}, nil
	case config.ChannelSlack:
		var tokens SlackSecret

		if err := factory.DecryptSecret(connection.Secret, &tokens); err != nil {
			return nil, err
		}

		return Slack{WebhookURL: tokens.WebhookURL, Client: factory.HTTPClient}, nil
	default:
		return nil, fmt.Errorf("unknown connection type %q", connection.Type)
	}
}

// SendTest sends a test message right away, for the "Send test" button.
func (factory Factory) SendTest(ctx context.Context, connection store.Connection, issueURL string) error {
	channel, err := factory.Channel(connection)

	if err != nil {
		return err
	}

	return channel.Send(ctx, Notification{
		Kind:     Test,
		Project:  "test",
		IssueURL: issueURL,
		Title:    fmt.Sprintf("Test message for %q: notifications reach this destination", connection.Name),
		Level:    "info",
		Time:     time.Now(),
	})
}

// matches reports whether a connection receives a notification: it is active
// and verified, covers the project, and the level is one it selected.
func matches(connection store.Connection, notification Notification) bool {
	if !connection.Active || !connection.Verified {
		return false
	}

	if !connection.AllProjects && !slices.Contains(connection.ProjectIDs, notification.ProjectID) {
		return false
	}

	return slices.Contains(connection.Levels, normalizeLevel(notification.Level))
}

// normalizeLevel maps levels SDKs may send beyond config.Levels ("log",
// "critical") to "error": better one message too many than a missed crash.
func normalizeLevel(level string) string {
	if slices.Contains(config.Levels, level) {
		return level
	}

	return "error"
}
