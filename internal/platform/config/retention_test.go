package config

import (
	"strings"
	"testing"
	"time"
)

func TestRetentionDefaults(t *testing.T) {
	config, err := loadFrom(env(nil))

	if err != nil {
		t.Fatal(err)
	}

	want := Retention{Events: 30 * 24 * time.Hour, Logs: 14 * 24 * time.Hour, EventsPerIssue: 100, MaxDatabaseBytes: 2 << 30, Issues: 365 * 24 * time.Hour, LogsPerMessage: 50}

	if config.Retention != want {
		t.Fatalf("defaults: %+v", config.Retention)
	}
}

func TestRetentionValues(t *testing.T) {
	config, err := loadFrom(env(map[string]string{
		"RETENTION_EVENTS":           "90d",
		"RETENTION_LOGS":             "0",
		"RETENTION_EVENTS_PER_ISSUE": "0",
		"MAX_DATABASE_SIZE":          "1.5gb",
		"RETENTION_ISSUES":           "0",
		"RETENTION_LOGS_PER_MESSAGE": "0",
	}))

	if err != nil {
		t.Fatal(err)
	}

	want := Retention{Events: 90 * 24 * time.Hour, MaxDatabaseBytes: 3 << 29}

	if config.Retention != want {
		t.Fatalf("values: %+v", config.Retention)
	}
}

func TestRetentionErrors(t *testing.T) {
	for name, value := range map[string]string{
		"RETENTION_EVENTS":           "forever",
		"RETENTION_LOGS":             "-1d",
		"RETENTION_EVENTS_PER_ISSUE": "-5",
		"MAX_DATABASE_SIZE":          "2 gigs",
		"RETENTION_ISSUES":           "1y",
		"RETENTION_LOGS_PER_MESSAGE": "lots",
	} {
		_, err := loadFrom(env(map[string]string{name: value}))

		if err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%q: got %v", name, value, err)
		}
	}
}

func TestFormatSize(t *testing.T) {
	for bytes, want := range map[int64]string{512: "512 B", 3 << 20: "3.0 MB", 2 << 30: "2.0 GB", 1536 << 20: "1.5 GB"} {
		if got := FormatSize(bytes); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", bytes, got, want)
		}
	}
}
