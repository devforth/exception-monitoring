package config

import (
	"strings"
	"testing"
)

func TestMaxDatabaseSize(t *testing.T) {
	for value, want := range map[string]int64{"": 2 << 30, "1.5gb": 3 << 29, "0": 0} {
		config, err := loadFrom(env(map[string]string{"MAX_DATABASE_SIZE": value}))

		if err != nil || config.Retention.MaxDatabaseBytes != want {
			t.Errorf("MAX_DATABASE_SIZE=%q: %d, %v", value, config.Retention.MaxDatabaseBytes, err)
		}
	}

	if _, err := loadFrom(env(map[string]string{"MAX_DATABASE_SIZE": "2 gigs"})); err == nil || !strings.Contains(err.Error(), "MAX_DATABASE_SIZE") {
		t.Errorf("2 gigs: %v", err)
	}
}

func TestFormatSize(t *testing.T) {
	for bytes, want := range map[int64]string{512: "512 B", 3 << 20: "3.0 MB", 2 << 30: "2.0 GB", 1536 << 20: "1.5 GB"} {
		if got := FormatSize(bytes); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", bytes, got, want)
		}
	}
}
