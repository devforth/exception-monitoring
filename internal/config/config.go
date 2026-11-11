// Package config loads server settings from environment variables.
package config

import (
	"fmt"
	"log/slog"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// Project is a source of events (e.g. "backend", "frontend") with its own DSN.
// ID is set explicitly in PROJECTS and is the number at the end of the DSN.
// It identifies the project's stored data, so it must never change once in use.
type Project struct {
	ID        int
	Name      string
	PublicKey string
}

type Config struct {
	Port     int
	DataDir  string
	LogLevel slog.Level
	Projects []Project
}

var (
	projectNameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]*$`)
	publicKeyRe   = regexp.MustCompile(`^[A-Za-z0-9]{16,64}$`)
)

// Load reads the configuration from the process environment.
func Load() (*Config, error) {
	return loadFrom(os.Getenv)
}

func loadFrom(getenv func(string) string) (*Config, error) {
	config := &Config{
		Port:     8000,
		DataDir:  "./data",
		LogLevel: slog.LevelInfo,
	}

	if value := getenv("PORT"); value != "" {
		port, err := strconv.Atoi(value)
		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("PORT: invalid port %q", value)
		}
		config.Port = port
	}

	if value := getenv("DATA_DIR"); value != "" {
		// The path goes into a SQLite "file:" DSN where these characters have a special meaning.
		if strings.ContainsAny(value, "?#%") {
			return nil, fmt.Errorf("DATA_DIR: path must not contain '?', '#' or '%%': %q", value)
		}
		config.DataDir = value
	}

	if value := getenv("LOG_LEVEL"); value != "" {
		if err := config.LogLevel.UnmarshalText([]byte(value)); err != nil {
			return nil, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}

	projects, err := parseProjects(getenv("PROJECTS"))
	if err != nil {
		return nil, fmt.Errorf("PROJECTS: %w", err)
	}
	config.Projects = projects

	return config, nil
}

// parseProjects parses "id:name:key,id:name:key". Empty input means no projects.
func parseProjects(raw string) ([]Project, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	var projects []Project
	seenIDs := map[int]bool{}
	seenNames := map[string]bool{}
	seenKeys := map[string]bool{}

	for index, entry := range strings.Split(raw, ",") {
		parts := strings.Split(strings.TrimSpace(entry), ":")
		if len(parts) != 3 {
			return nil, fmt.Errorf("entry %d %q: expected id:name:public_key", index+1, entry)
		}
		rawID, name, key := parts[0], parts[1], parts[2]

		id, err := strconv.Atoi(rawID)
		if err != nil || id < 1 {
			return nil, fmt.Errorf("entry %d: invalid id %q (positive integer)", index+1, rawID)
		}
		if seenIDs[id] {
			return nil, fmt.Errorf("duplicate project id %d", id)
		}
		if !projectNameRe.MatchString(name) {
			return nil, fmt.Errorf("entry %d: invalid name %q (lowercase letters, digits, '-', '_')", index+1, name)
		}
		if !publicKeyRe.MatchString(key) {
			return nil, fmt.Errorf("project %q: public key must be 16-64 letters or digits", name)
		}
		if seenNames[name] {
			return nil, fmt.Errorf("duplicate project name %q", name)
		}
		if seenKeys[key] {
			return nil, fmt.Errorf("project %q: public key is already used by another project", name)
		}
		seenIDs[id] = true
		seenNames[name] = true
		seenKeys[key] = true
		projects = append(projects, Project{ID: id, Name: name, PublicKey: key})
	}
	return projects, nil
}
