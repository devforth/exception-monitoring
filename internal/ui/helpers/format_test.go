package helpers

import (
	"testing"
	"time"
)

func TestTimeAgo(t *testing.T) {
	now := time.Now()

	for _, test := range []struct {
		ago  time.Duration
		want string
	}{
		{59 * time.Second, "just now"},
		{time.Minute + time.Second, "1m ago"},
		{59*time.Minute + 59*time.Second, "59m ago"},
		{time.Hour + time.Second, "1h ago"},
		{24*time.Hour + time.Second, "1d ago"},
		{29 * 24 * time.Hour, "29d ago"},
	} {
		if got := TimeAgo(now.Add(-test.ago)); got != test.want {
			t.Errorf("TimeAgo(-%v) = %q, want %q", test.ago, got, test.want)
		}
	}

	// Times to come (the next retry of a message).
	for ahead, want := range map[time.Duration]string{30 * time.Second: "just now", 4*time.Minute + 10*time.Second: "in 4m", 2*time.Hour + time.Minute: "in 2h"} {
		if got := TimeAgo(now.Add(ahead)); got != want {
			t.Errorf("TimeAgo(+%v) = %q, want %q", ahead, got, want)
		}
	}

	old := now.Add(-31 * 24 * time.Hour)

	if got := TimeAgo(old); got != old.UTC().Format("2006-01-02") {
		t.Errorf("TimeAgo a month ago = %q, want the date", got)
	}
}

func TestShort(t *testing.T) {
	if got := Short("0123456789abcdef", 8); got != "01234567" {
		t.Errorf("Short = %q", got)
	}

	if got := Short("abc", 8); got != "abc" {
		t.Errorf("Short of a short text = %q", got)
	}
}

func TestMaskSecret(t *testing.T) {
	for secret, want := range map[string]string{
		"":      "",
		"short": "…",
		"https://hooks.slack.com/services/T0001/B0002/abcdefghijklmnopqrstuvwx": "https://hooks.slack.com/services/…uvwx",
		// Split so secret scanners do not take this made-up token for a real one.
		"xoxb-" + "1234567890-0987654321-AbCdEfGhIjKlMnOp": "xoxb-1234567890-…MnOp", // the team id is not secret
		"123456789:AAHdqTcvCH1vGWJxfSeofSAs0K5PALDsaw":     "123456789:…Dsaw",
	} {
		if got := MaskSecret(secret); got != want {
			t.Errorf("MaskSecret(%q) = %q, want %q", secret, got, want)
		}
	}
}
