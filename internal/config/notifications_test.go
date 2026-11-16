package config

import (
	"strings"
	"testing"
	"time"
)

const secretKey = "0123456789abcdef0123456789abcdef"

func TestNotificationsDefaults(t *testing.T) {
	config, err := loadFrom(env(nil))

	if err != nil {
		t.Fatal(err)
	}

	notifications := config.Notifications

	if strings.Join(notifications.ChannelTypes, ",") != "email,telegram,slack" || notifications.SMTP.Enabled() || notifications.SecretKey != "" ||
		notifications.RetryFor != 24*time.Hour || notifications.KeepSent != 24*time.Hour || notifications.KeepFailed != 30*24*time.Hour {
		t.Fatalf("defaults: %+v", notifications)
	}

	// Allowed by default, but nothing they need is configured yet.
	for _, channelType := range AllChannelTypes {
		if available, _ := notifications.ChannelAvailable(channelType); available {
			t.Errorf("%s must not be available without its settings", channelType)
		}
	}
}

func TestNotificationsValues(t *testing.T) {
	config, err := loadFrom(env(map[string]string{
		"PUBLIC_URL":           "https://errors.example.com/",
		"NOTIFY_CHANNEL_TYPES": "Email, slack, email",
		"SECRET_KEY":           secretKey,
		"SMTP_HOST":            "smtp.mailgun.org",
		"SMTP_USERNAME":        "postmaster@mg.example.com",
		"SMTP_PASSWORD":        "secret",
		"EMAIL_FROM":           "Errors <errors@example.com>",
		"NOTIFY_RETRY_FOR":     "6h",
		"NOTIFY_KEEP_SENT":     "2d",
		"NOTIFY_KEEP_FAILED":   "90d",
	}))

	if err != nil {
		t.Fatal(err)
	}

	notifications := config.Notifications

	// The trailing slash is dropped: links are built as PublicURL + "/issues/1".
	if notifications.PublicURL != "https://errors.example.com" || strings.Join(notifications.ChannelTypes, ",") != "email,slack" {
		t.Fatalf("public url %q, types %v", notifications.PublicURL, notifications.ChannelTypes)
	}

	if notifications.RetryFor != 6*time.Hour || notifications.KeepSent != 48*time.Hour || notifications.KeepFailed != 90*24*time.Hour {
		t.Fatalf("retry %s, keep sent %s, keep failed %s", notifications.RetryFor, notifications.KeepSent, notifications.KeepFailed)
	}

	for channelType, want := range map[string]bool{ChannelEmail: true, ChannelSlack: true, ChannelTelegram: false} {
		if available, reason := notifications.ChannelAvailable(channelType); available != want {
			t.Errorf("%s available = %v (%s), want %v", channelType, available, reason, want)
		}
	}
}

func TestChannelAvailabilityReasons(t *testing.T) {
	testCases := []struct {
		name, channelType, wantReason string
		notifications                 Notifications
	}{
		{"not allowed", ChannelSlack, "NOTIFY_CHANNEL_TYPES", Notifications{ChannelTypes: []string{ChannelEmail}, SecretKey: secretKey}},
		{"email without smtp", ChannelEmail, "SMTP_HOST", Notifications{ChannelTypes: AllChannelTypes}},
		{"telegram without key", ChannelTelegram, "SECRET_KEY", Notifications{ChannelTypes: AllChannelTypes}},
	}

	for _, testCase := range testCases {
		available, reason := testCase.notifications.ChannelAvailable(testCase.channelType)

		if available || !strings.Contains(reason, testCase.wantReason) {
			t.Errorf("%s: available=%v reason=%q", testCase.name, available, reason)
		}
	}
}

func TestNotificationsErrors(t *testing.T) {
	smtp := map[string]string{"SMTP_HOST": "smtp.example.com", "EMAIL_FROM": "errors@example.com"}

	with := func(values map[string]string, key, value string) map[string]string {
		result := map[string]string{key: value}

		for name, existing := range values {
			if name != key {
				result[name] = existing
			}
		}

		return result
	}

	testCases := []struct {
		name    string
		values  map[string]string
		wantErr string
	}{
		{"public url without scheme", map[string]string{"PUBLIC_URL": "errors.example.com"}, "PUBLIC_URL"},
		{"unknown channel type", map[string]string{"NOTIFY_CHANNEL_TYPES": "email,discord"}, "unknown type"},
		{"short secret key", map[string]string{"SECRET_KEY": "short"}, "SECRET_KEY"},
		{"smtp without sender", with(smtp, "EMAIL_FROM", ""), "EMAIL_FROM"},
		{"smtp bad port", with(smtp, "SMTP_PORT", "smtp"), "SMTP_PORT"},
		{"keep duration without unit", map[string]string{"NOTIFY_KEEP_SENT": "30"}, "NOTIFY_KEEP_SENT"},
		{"keep duration zero", map[string]string{"NOTIFY_KEEP_FAILED": "0d"}, "NOTIFY_KEEP_FAILED"},
		{"retry duration negative", map[string]string{"NOTIFY_RETRY_FOR": "-1h"}, "NOTIFY_RETRY_FOR"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := loadFrom(env(testCase.values))

			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("got error %v, want it to contain %q", err, testCase.wantErr)
			}
		})
	}

	// "none" turns every channel off.
	config, err := loadFrom(env(map[string]string{"NOTIFY_CHANNEL_TYPES": "none"}))

	if err != nil || len(config.Notifications.ChannelTypes) != 0 {
		t.Fatalf("none: %v %v", config, err)
	}
}
