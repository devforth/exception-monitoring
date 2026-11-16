package config

import (
	"fmt"
	"strings"
	"testing"
)

func TestRecording(t *testing.T) {
	defaults, err := loadFrom(env(nil))

	if err != nil {
		t.Fatal(err)
	}

	if fmt.Sprint(defaults.Recording.EventLevels) != "[error fatal]" || defaults.Recording.LogLevels != nil {
		t.Fatalf("defaults: %+v", defaults.Recording)
	}

	values, err := loadFrom(env(map[string]string{"RECORD_EVENT_LEVELS": "all", "RECORD_LOG_LEVELS": "Warn, error, warn"}))

	if err != nil {
		t.Fatal(err)
	}

	if values.Recording.EventLevels != nil || fmt.Sprint(values.Recording.LogLevels) != "[warn error]" {
		t.Fatalf("values: %+v", values.Recording)
	}

	if none, _ := ParseLevels("none", LogLevels); none == nil || len(none) != 0 {
		t.Fatal(`"none" must be an empty list, not nil (which records every level)`)
	}

	for name, value := range map[string]string{"RECORD_EVENT_LEVELS": "warn", "RECORD_LOG_LEVELS": "warning"} {
		if _, err := loadFrom(env(map[string]string{name: value})); err == nil || !strings.Contains(err.Error(), name) {
			t.Errorf("%s=%s: %v", name, value, err)
		}
	}
}

func TestLevelNames(t *testing.T) {
	for level, want := range map[string]string{"critical": "fatal", "warn": "warning", "WARNING": "warning", "error": "error", "trace": "debug", "log": "info", "weird": "error"} {
		if got := EventLevel(level); got != want {
			t.Errorf("EventLevel(%q) = %q, want %q", level, got, want)
		}
	}

	for level, want := range map[string]string{"warning": "warn", "critical": "fatal", "trace": "trace", "weird": "info"} {
		if got := LogLevel(level); got != want {
			t.Errorf("LogLevel(%q) = %q, want %q", level, got, want)
		}
	}
}
