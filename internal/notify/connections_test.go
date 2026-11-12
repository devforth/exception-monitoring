package notify

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/secret"
	"github.com/devforth/exception-monitoring/internal/store"
)

func testFactory(t *testing.T) Factory {
	t.Helper()

	box, err := secret.New("0123456789abcdef0123456789abcdef")

	if err != nil {
		t.Fatal(err)
	}

	return Factory{Box: box}
}

func TestChatTokensAreStoredEncrypted(t *testing.T) {
	factory := testFactory(t)

	encrypted, err := factory.EncryptSecret(TelegramSecret{BotToken: "123:SECRET", ChatID: "-100"})

	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(encrypted, "SECRET") {
		t.Fatal("the stored secret must not contain the token")
	}

	var tokens TelegramSecret

	if err := factory.DecryptSecret(encrypted, &tokens); err != nil || tokens.BotToken != "123:SECRET" || tokens.ChatID != "-100" {
		t.Fatalf("decrypted %+v, %v", tokens, err)
	}

	// Without SECRET_KEY chat connections cannot be used.
	if _, err := (Factory{}).EncryptSecret(tokens); !errors.Is(err, ErrNoSecretKey) {
		t.Fatalf("without a key: %v", err)
	}
}

func TestSendTest(t *testing.T) {
	server, request, payload := recordingServer(t, http.StatusOK, nil, `{"ok":true}`)
	factory := testFactory(t)
	factory.telegramURL = server.URL

	encrypted, _ := factory.EncryptSecret(TelegramSecret{BotToken: "123:SECRET", ChatID: "42"})
	connection := store.Connection{Name: "Ops chat", Type: config.ChannelTelegram, Secret: encrypted}

	if err := factory.SendTest(context.Background(), connection, "https://errors.example.com/notifications"); err != nil {
		t.Fatal(err)
	}

	var message struct {
		ChatID string `json:"chat_id"`
		Text   string `json:"text"`
	}

	_ = json.Unmarshal(*payload, &message)

	if request.URL.Path != "/bot123:SECRET/sendMessage" || message.ChatID != "42" || !strings.Contains(message.Text, "Test message") || !strings.Contains(message.Text, "Ops chat") {
		t.Fatalf("path %s, message %+v", request.URL.Path, message)
	}
}

func TestFactoryChannels(t *testing.T) {
	factory := testFactory(t)
	webhook, _ := factory.EncryptSecret(SlackSecret{WebhookURL: "https://hooks.slack.com/services/x"})

	slack, err := factory.Channel(store.Connection{Type: config.ChannelSlack, Secret: webhook})

	if err != nil || slack.(Slack).WebhookURL != "https://hooks.slack.com/services/x" {
		t.Fatalf("slack: %+v %v", slack, err)
	}

	if _, err := factory.Channel(store.Connection{Type: config.ChannelEmail, Address: "ops@example.com"}); err == nil {
		t.Fatal("email without SMTP must fail")
	}

	factory.SMTP = config.SMTP{Host: "smtp.example.com", Port: 587, From: "errors@example.com"}

	email, err := factory.Channel(store.Connection{Type: config.ChannelEmail, Address: "ops@example.com"})

	if err != nil || email.(Email).To != "ops@example.com" {
		t.Fatalf("email: %+v %v", email, err)
	}

	// A secret encrypted with another key (SECRET_KEY changed) is reported, not sent garbage.
	other, _ := secret.New(strings.Repeat("y", 40))
	foreign, _ := (Factory{Box: other}).EncryptSecret(SlackSecret{WebhookURL: "https://x"})

	if _, err := factory.Channel(store.Connection{Type: config.ChannelSlack, Secret: foreign}); !errors.Is(err, secret.ErrDecrypt) {
		t.Fatalf("foreign secret: %v", err)
	}
}
