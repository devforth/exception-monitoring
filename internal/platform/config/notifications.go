package config

import (
	"fmt"
	"net/mail"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Notifications are the server-wide settings of alerts. Where alerts go
// (email addresses, Telegram chats, Slack webhooks) is managed in the UI and
// stored in the database; the environment provides what the types need: SMTP
// for email, SECRET_KEY to encrypt chat tokens in the database (generated in
// the data directory when not set).
type Notifications struct {
	// PublicURL is where the server is reachable (e.g. https://errors.example.com),
	// used for links in notifications and for the DSNs shown on the Projects
	// page. Empty means messages carry no link and DSNs use the address the
	// UI was opened with.
	PublicURL string

	// SecretKey encrypts chat tokens and webhook URLs in the database:
	// SECRET_KEY, or the key generated in the data directory at startup.
	SecretKey string

	SMTP SMTP

	// RetryFor is how long a message that cannot be delivered is retried before
	// it is marked failed. KeepSent and KeepFailed are how long delivered and
	// failed messages stay in the outbox before they are deleted.
	RetryFor   time.Duration
	KeepSent   time.Duration
	KeepFailed time.Duration
}

// ImplicitTLSPort is the SMTP port that speaks TLS from the start (SMTPS).
const ImplicitTLSPort = 465

// SMTP is the mail server for email notifications and verification codes.
// Any provider works: Mailtrap, Mailgun, Amazon SES, Gmail, a local Postfix.
type SMTP struct {
	Host     string
	Port     int // ImplicitTLSPort means implicit TLS; other ports use STARTTLS when offered
	Username string
	Password string
	From     string
}

// Levels are the event levels from lowest to highest, as Sentry SDKs send them.
var Levels = []string{"debug", "info", "warning", "error", "fatal"}

// Channel types a notification connection can have.
const (
	ChannelEmail    = "email"
	ChannelTelegram = "telegram"
	ChannelSlack    = "slack"
)

// Patterns of the chat fields of a connection. They are also the pattern
// attributes of the form, which browsers read in the "v" mode, hence the
// escaped "-" in classes; the server anchors them.
const (
	TelegramTokenPattern = `\d+:[A-Za-z0-9_\-]{20,}`
	TelegramChatPattern  = `-?\d+|@[A-Za-z0-9_]{5,}`
	SlackTokenPattern    = `xoxb-[A-Za-z0-9\-]{10,}`
	SlackChannelPattern  = `#?[A-Za-z0-9._\-]{1,80}`
)

// AllChannelTypes is every supported type.
var AllChannelTypes = []string{ChannelEmail, ChannelTelegram, ChannelSlack}

const (
	DefaultSMTPPort = 587

	DefaultNotifyRetryFor   = 24 * time.Hour
	DefaultNotifyKeepSent   = 24 * time.Hour
	DefaultNotifyKeepFailed = 30 * 24 * time.Hour

	// MinSecretKeyLength matches secret.MinKeyLength.
	MinSecretKeyLength = 32
)

// Enabled reports whether a mail server is configured.
func (smtp SMTP) Enabled() bool { return smtp.Host != "" }

// ChannelAvailable reports whether new connections of this type can be
// created, and if not, what is missing, for the UI.
func (notifications Notifications) ChannelAvailable(channelType string) (bool, string) {
	switch channelType {
	case ChannelEmail:
		if !notifications.SMTP.Enabled() {
			return false, "set SMTP_HOST and EMAIL_FROM"
		}
	case ChannelTelegram, ChannelSlack:
		// Their tokens are stored encrypted, so they need the key; the server
		// always has one (SECRET_KEY or generated), tests may not.
		if notifications.SecretKey == "" {
			return false, "set SECRET_KEY"
		}
	default:
		return false, "unknown type"
	}

	return true, ""
}

func parseNotifications(getenv func(string) string) (Notifications, error) {
	notifications := Notifications{
		SMTP:       SMTP{Port: DefaultSMTPPort},
		RetryFor:   DefaultNotifyRetryFor,
		KeepSent:   DefaultNotifyKeepSent,
		KeepFailed: DefaultNotifyKeepFailed,
	}

	for _, setting := range []struct {
		key    string
		target *time.Duration
	}{
		{"NOTIFY_RETRY_FOR", &notifications.RetryFor},
		{"NOTIFY_KEEP_SENT", &notifications.KeepSent},
		{"NOTIFY_KEEP_FAILED", &notifications.KeepFailed},
	} {
		if value := strings.TrimSpace(getenv(setting.key)); value != "" {
			duration, err := parseDuration(value)

			if err != nil {
				return notifications, fmt.Errorf("%s: %w", setting.key, err)
			}

			*setting.target = duration
		}
	}

	if value := strings.TrimRight(strings.TrimSpace(getenv("PUBLIC_URL")), "/"); value != "" {
		if err := checkHTTPURL(value); err != nil {
			return notifications, fmt.Errorf("PUBLIC_URL: %w", err)
		}

		notifications.PublicURL = value
	}

	if value := getenv("SECRET_KEY"); value != "" {
		if len(value) < MinSecretKeyLength {
			return notifications, fmt.Errorf("SECRET_KEY: must be at least %d characters (e.g. openssl rand -hex 32)", MinSecretKeyLength)
		}

		notifications.SecretKey = value
	}

	smtp, err := parseSMTP(getenv, notifications.SMTP)
	notifications.SMTP = smtp

	return notifications, err
}

func parseSMTP(getenv func(string) string, smtp SMTP) (SMTP, error) {
	smtp.Host = strings.TrimSpace(getenv("SMTP_HOST"))

	if smtp.Host == "" {
		return smtp, nil
	}

	if value := getenv("SMTP_PORT"); value != "" {
		port, err := strconv.Atoi(value)

		if err != nil || port < 1 || port > 65535 {
			return smtp, fmt.Errorf("SMTP_PORT: invalid port %q", value)
		}

		smtp.Port = port
	}

	smtp.Username = getenv("SMTP_USERNAME")
	smtp.Password = getenv("SMTP_PASSWORD")

	from := strings.TrimSpace(getenv("EMAIL_FROM"))

	if _, err := mail.ParseAddress(from); err != nil {
		return smtp, fmt.Errorf("EMAIL_FROM: a sender address is required with SMTP_HOST (e.g. errors@example.com): %w", err)
	}

	smtp.From = from

	return smtp, nil
}

// parseDuration accepts days ("30d") as well as Go durations ("12h", "90m").
// Go has no "d" unit, but retention periods are naturally given in days.
func parseDuration(value string) (time.Duration, error) {
	var (
		duration time.Duration
		err      error
	)

	if days, ok := strings.CutSuffix(value, "d"); ok {
		count, parseErr := strconv.Atoi(days)
		duration, err = time.Duration(count)*24*time.Hour, parseErr
	} else {
		duration, err = time.ParseDuration(value)
	}

	if err != nil || duration <= 0 {
		return 0, fmt.Errorf("invalid duration %q (e.g. 30d, 12h, 90m)", value)
	}

	return duration, nil
}

func checkHTTPURL(value string) error {
	parsed, err := url.Parse(value)

	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("invalid URL %q (expected http:// or https://)", value)
	}

	return nil
}

// CheckHTTPURL validates a webhook URL entered in the UI.
func CheckHTTPURL(value string) error { return checkHTTPURL(value) }
