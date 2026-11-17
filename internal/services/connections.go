package services

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/platform/text"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Connections: the notification connections, what tells them apart and
// names them, and their delivery health.
type Connections struct {
	logger        *slog.Logger
	store         store.Store
	factory       notify.Factory
	projects      config.ProjectList
	notifications config.Notifications
	recording     config.Recording
	now           func() time.Time
}

func NewConnections(logger *slog.Logger, database store.Store, factory notify.Factory, projects config.ProjectList, settings *config.Config) *Connections {
	return &Connections{logger: logger, store: database, factory: factory, projects: projects, notifications: settings.Notifications, recording: settings.Recording, now: time.Now}
}

// New is what a new connection of channelType starts with (store.NewConnection).
func (service *Connections) New(channelType string) store.Connection {
	return store.NewConnection(channelType, service.recording)
}

// List returns every connection.
func (service *Connections) List(ctx context.Context) ([]store.Connection, error) {
	return service.store.ListConnections(ctx)
}

// DeliveryHealth sums up the delivery of each connection, by connection id.
func (service *Connections) DeliveryHealth(ctx context.Context) (map[string]store.DeliveryHealth, error) {
	return service.store.DeliveryHealthByChannel(ctx)
}

// Get reads a connection; store.ErrNotFound when it does not exist.
func (service *Connections) Get(ctx context.Context, id int64) (store.Connection, error) {
	return service.store.GetConnection(ctx, id)
}

// create saves a new connection and returns its id. chatSecret is the
// encrypted secret of a chat (empty for email): the chat's destination is
// created first.
func (service *Connections) create(ctx context.Context, connection store.Connection, chatSecret string) (int64, error) {
	if chatSecret != "" {
		// Chats need no confirmation: whoever has the token owns the chat.
		now := service.now()
		destinationID, err := service.store.CreateDestination(ctx, store.Destination{Type: connection.Type, Secret: chatSecret, Verified: true, VerifiedAt: now}, now)

		if err != nil {
			return 0, err
		}

		connection.DestinationID = destinationID
	}

	return service.store.CreateConnection(ctx, connection, service.now())
}

// update saves a connection. newChatSecret is the encrypted chat secret read
// from the form (empty for email); the destination gets it when it differs
// from the stored one.
func (service *Connections) update(ctx context.Context, connection store.Connection, newChatSecret string) error {
	if newChatSecret != "" && newChatSecret != connection.Secret {
		if err := service.store.UpdateDestinationSecret(ctx, connection.DestinationID, newChatSecret, service.now()); err != nil {
			return err
		}
	}

	return service.saved(ctx, connection)
}

// SetActive turns a connection on or off; an unconfirmed address cannot be
// turned on (store.ErrNotVerified).
func (service *Connections) SetActive(ctx context.Context, connection store.Connection, active bool) error {
	if active && !connection.Verified {
		return store.ErrNotVerified
	}

	connection.Active = active

	return service.saved(ctx, connection)
}

// RetryNow sends the waiting messages of a connection again now, without
// waiting for the next step of the retry schedule: for when the problem was
// fixed outside this server (a scope added in Slack, the bot invited).
func (service *Connections) RetryNow(ctx context.Context, id int64) error {
	return service.store.RetryOutboxNow(ctx, strconv.FormatInt(id, 10), service.now())
}

// saved stores a changed connection and retries its waiting messages now:
// a save is usually the fix for why they failed.
func (service *Connections) saved(ctx context.Context, connection store.Connection) error {
	if err := service.store.UpdateConnection(ctx, connection, service.now()); err != nil {
		return err
	}

	if !connection.Active {
		return nil
	}

	return service.store.RetryOutboxNow(ctx, strconv.FormatInt(connection.ID, 10), service.now())
}

// Delete removes a connection; one already gone (deleted from another tab)
// is not an error. Its destination stays, so a confirmed address stays
// confirmed.
func (service *Connections) Delete(ctx context.Context, id int64) error {
	if err := service.store.DeleteConnection(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
		return err
	}

	return nil
}

// DefaultName names a connection saved without a name: an email
// address by its part before "@", a Telegram connection by its chat (asked
// from Telegram), a Slack webhook "Slack" (a webhook does not tell its
// channel). secret is the encrypted chat secret.
func (service *Connections) DefaultName(ctx context.Context, connection store.Connection, secret string) string {
	switch connection.Type {
	case config.ChannelEmail:
		return text.TruncateRunes(text.EmailLocalPart(connection.Address), config.MaxConnectionName)
	case config.ChannelTelegram:
		var tokens notify.TelegramSecret

		if service.factory.DecryptSecret(secret, &tokens) != nil {
			return "Telegram"
		}

		lookupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)

		defer cancel()

		title, err := service.factory.TelegramChatTitle(lookupCtx, tokens)

		if err != nil || strings.TrimSpace(title) == "" {
			service.logger.Warn("telegram chat name not available", "err", err)

			return "Telegram " + tokens.ChatID
		}

		return text.TruncateRunes(strings.TrimSpace(title), config.MaxConnectionName)
	default:
		return "Slack"
	}
}

// destinationKey is what makes two connections send to the same place: the
// email address, the Telegram chat or the Slack webhook. Chat secrets are
// encrypted with a random nonce, so they are compared decrypted.
func (service *Connections) destinationKey(connection store.Connection) string {
	switch connection.Type {
	case config.ChannelEmail:
		return strings.ToLower(connection.Address)
	case config.ChannelTelegram:
		var telegram notify.TelegramSecret

		if service.factory.DecryptSecret(connection.Secret, &telegram) == nil {
			return telegram.ChatID
		}
	case config.ChannelSlack:
		var slack notify.SlackSecret

		if service.factory.DecryptSecret(connection.Secret, &slack) == nil {
			return slack.Key()
		}
	}

	return ""
}

// FindDuplicate returns the connection other than exceptID that already
// sends to key (see destinationKey), or nil.
func (service *Connections) FindDuplicate(ctx context.Context, channelType, key string, exceptID int64) (*Duplicate, error) {
	if key == "" {
		return nil, nil
	}

	connections, err := service.store.ListConnections(ctx)

	if err != nil {
		return nil, err
	}

	for _, connection := range connections {
		if connection.ID != exceptID && connection.Type == channelType && service.destinationKey(connection) == key {
			return &Duplicate{ID: connection.ID, Name: connection.Name}, nil
		}
	}

	return nil, nil
}
