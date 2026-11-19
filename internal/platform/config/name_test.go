package config

import (
	"strings"
	"testing"
)

func TestAppName(t *testing.T) {
	for value, want := range map[string]string{"": DefaultAppName, "  Acme   Errors ": "Acme Errors"} {
		if got, err := parseAppName(value); err != nil || got != want {
			t.Errorf("APP_NAME %q: %q %v, want %q", value, got, err, want)
		}
	}

	if _, err := parseAppName(strings.Repeat("a", 61)); err == nil {
		t.Error("a name over 60 characters accepted")
	}

	for name, want := range map[string]string{
		"Exception Monitoring": "exception-monitoring",
		"Acme Errors (prod)":   "acme-errors-prod",
		"my_app 2":             "my_app-2",
		"Моніторинг":           "exception-monitoring", // nothing agents accept is left
	} {
		if got := AppSlug(name); got != want {
			t.Errorf("AppSlug(%q) = %q, want %q", name, got, want)
		}
	}
}
