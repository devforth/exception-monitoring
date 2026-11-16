package notify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/platform/secret"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Secrets of chat connections, stored as encrypted JSON in Connection.Secret.
type TelegramSecret struct {
	BotToken string `json:"bot_token"`
	ChatID   string `json:"chat_id"`
}

// SlackSecret: an incoming webhook, or a bot (Bot User OAuth Token with
// chat:write, invited to Channel). With a bot, messages are posted by the bot
// and the agent prompt goes in a thread under each error; a webhook cannot
// do that, it does not tell which message it posted.
type SlackSecret struct {
	WebhookURL string `json:"webhook_url,omitempty"`
	BotToken   string `json:"bot_token,omitempty"`
	Channel    string `json:"channel,omitempty"`
}

// Key is what makes two Slack connections post to the same place: the
// channel of a bot, else the webhook.
func (secret SlackSecret) Key() string {
	if secret.BotToken != "" {
		return "channel:" + secret.Channel
	}

	return secret.WebhookURL
}

// Factory turns stored connections into channels: it holds what they share
// (the SMTP server, the encryption key).
type Factory struct {
	// AppName (APP_NAME) is how messages name this server.
	AppName string
	SMTP    config.SMTP
	Box     *secret.Box // nil without SECRET_KEY: chat connections cannot be used
	// HTTPClient is used by chat channels; nil means http.DefaultClient.
	HTTPClient *http.Client
	// TelegramURL replaces the Telegram API (tests point it to a fake
	// server); empty means https://api.telegram.org.
	TelegramURL string
	// SlackURL replaces the Slack Web API the same way; empty means
	// https://slack.com/api.
	SlackURL string
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

func (factory Factory) telegram(tokens TelegramSecret) Telegram {
	return Telegram{BotToken: tokens.BotToken, ChatID: tokens.ChatID, AppName: factory.AppName, Client: factory.HTTPClient, apiURL: factory.TelegramURL}
}

// TelegramChatTitle asks Telegram for the name of the chat of these tokens,
// to name a connection the user left unnamed.
func (factory Factory) TelegramChatTitle(ctx context.Context, tokens TelegramSecret) (string, error) {
	return factory.telegram(tokens).ChatTitle(ctx)
}

// Channel builds the channel that delivers to a connection; when the
// connection hides issue links, its messages carry none.
func (factory Factory) Channel(connection store.Connection) (Channel, error) {
	channel, err := factory.channel(connection)

	if err != nil || !connection.HideIssueLink {
		return channel, err
	}

	return withoutIssueLinks{channel}, nil
}

// withoutIssueLinks drops the links to the panel from every message: for
// chats that people outside the team may read, the panel address stays out.
type withoutIssueLinks struct {
	Channel
}

func (channel withoutIssueLinks) Send(ctx context.Context, notification Notification) error {
	notification.IssueURL = ""

	return channel.Channel.Send(ctx, notification)
}

func (channel withoutIssueLinks) SendSummary(ctx context.Context, summary Summary) error {
	items := slices.Clone(summary.Items)

	for index := range items {
		items[index].IssueURL = ""
	}

	summary.Items = items

	return channel.Channel.SendSummary(ctx, summary)
}

func (factory Factory) channel(connection store.Connection) (Channel, error) {
	switch connection.Type {
	case config.ChannelEmail:
		if !factory.SMTP.Enabled() {
			return nil, errors.New("SMTP is not configured")
		}

		return factory.Email(connection.Address), nil
	case config.ChannelTelegram:
		var tokens TelegramSecret

		if err := factory.DecryptSecret(connection.Secret, &tokens); err != nil {
			return nil, err
		}

		return factory.telegram(tokens), nil
	case config.ChannelSlack:
		var tokens SlackSecret

		if err := factory.DecryptSecret(connection.Secret, &tokens); err != nil {
			return nil, err
		}

		return Slack{
			WebhookURL: tokens.WebhookURL, BotToken: tokens.BotToken, Channel: tokens.Channel, AppName: factory.AppName,
			Client: factory.HTTPClient, apiURL: factory.SlackURL,
		}, nil
	default:
		return nil, fmt.Errorf("unknown connection type %q", connection.Type)
	}
}

// Email is the email channel to address (also for confirmation codes).
func (factory Factory) Email(address string) Email {
	return Email{SMTP: factory.SMTP, To: address, AppName: factory.AppName}
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

	return slices.Contains(connection.Levels, config.EventLevel(notification.Level))
}
