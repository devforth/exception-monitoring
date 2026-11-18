package logging

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
)

func TestPretty(t *testing.T) {
	var out bytes.Buffer

	logger := slog.New(NewPretty(&out, slog.LevelInfo, false))
	logger.Debug("hidden")
	logger.Info("server started", "url", "http://localhost:8000", "projects", 2, "empty", "")
	logger.With("request", slog.GroupValue(slog.String("method", "GET"))).Warn("slow", "err", errors.New("timeout"))

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")

	if len(lines) != 2 {
		t.Fatalf("lines:\n%s", out.String())
	}

	// A time, the level, the message padded so fields line up; empty fields left out.
	if want := " INFO  " + padded("server started") + "  url=http://localhost:8000  projects=2"; !strings.HasSuffix(lines[0], want) || len(lines[0]) != len("15:04:05")+len(want) {
		t.Errorf("got  %q\nwant %q", lines[0], want)
	}

	if want := " WARN  " + padded("slow") + "  request.method=GET  err=timeout"; !strings.HasSuffix(lines[1], want) {
		t.Errorf("got  %q\nwant %q", lines[1], want)
	}
}

func padded(message string) string {
	return message + strings.Repeat(" ", messageWidth-len(message))
}

func TestPrettyColor(t *testing.T) {
	var out bytes.Buffer

	slog.New(NewPretty(&out, slog.LevelInfo, true)).Error("failed", "err", "boom")

	if !strings.Contains(out.String(), red+bold+"ERROR"+reset) || !strings.Contains(out.String(), red+"boom"+reset) {
		t.Errorf("no colors: %q", out.String())
	}
}

// A line wider than the terminal puts each field under the message.
func TestPrettyWraps(t *testing.T) {
	var out bytes.Buffer

	pretty := NewPretty(&out, slog.LevelInfo, false)
	pretty.width = func() int { return 60 }
	logger := slog.New(pretty)

	logger.Info("settings", "events", "30d", "logs", "14d", "issues", "365d", "events_per_issue", 100)
	logger.Info("short", "a", 1)

	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	indent := strings.Repeat(" ", len("15:04:05 LEVEL "))

	if len(lines) != 6 || !strings.HasSuffix(lines[0], " INFO  settings") || lines[1] != indent+"events=30d" || lines[4] != indent+"events_per_issue=100" {
		t.Fatalf("wrapped:\n%s", out.String())
	}

	if !strings.HasSuffix(lines[5], " INFO  "+padded("short")+"  a=1") {
		t.Errorf("a line that fits is wrapped: %q", lines[5])
	}
}

// Settings (Block) always go one field per line; other formats leave the mark out.
func TestBlock(t *testing.T) {
	var out bytes.Buffer

	slog.New(NewPretty(&out, slog.LevelInfo, false)).Info("channels", Block, "email", "on", "slack", "off: SECRET_KEY")

	indent := strings.Repeat(" ", len("15:04:05 LEVEL "))

	if lines := strings.Split(strings.TrimSpace(out.String()), "\n"); len(lines) != 3 || lines[1] != indent+"email=on" || lines[2] != indent+"slack=off: SECRET_KEY" {
		t.Errorf("block:\n%s", out.String())
	}

	file, err := os.CreateTemp(t.TempDir(), "log")

	if err != nil {
		t.Fatal(err)
	}

	New(file, FormatText, slog.LevelInfo).Info("channels", Block, "email", "on")

	written, _ := os.ReadFile(file.Name())

	if strings.Contains(string(written), blockKey) || !strings.Contains(string(written), "email=on") {
		t.Errorf("text: %s", written)
	}
}
