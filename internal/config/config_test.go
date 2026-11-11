package config

import (
	"log/slog"
	"strings"
	"testing"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestLoadDefaults(t *testing.T) {
	config, err := loadFrom(env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if config.Port != 8000 || config.DataDir != "./data" || config.LogLevel != slog.LevelInfo || config.Projects != nil {
		t.Fatalf("unexpected defaults: %+v", config)
	}
}

func TestLoadValues(t *testing.T) {
	config, err := loadFrom(env(map[string]string{
		"PORT":      "9000",
		"DATA_DIR":  "/data",
		"LOG_LEVEL": "debug",
		// IDs are explicit, so they don't have to follow list order or be contiguous.
		"PROJECTS": "3:frontend:0123456789abcdef0123, 1:backend:a1b2c3d4e5f6a7b8",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if config.Port != 9000 || config.DataDir != "/data" || config.LogLevel != slog.LevelDebug {
		t.Fatalf("unexpected config: %+v", config)
	}
	want := []Project{
		{ID: 3, Name: "frontend", PublicKey: "0123456789abcdef0123"},
		{ID: 1, Name: "backend", PublicKey: "a1b2c3d4e5f6a7b8"},
	}
	if len(config.Projects) != len(want) {
		t.Fatalf("got %d projects, want %d", len(config.Projects), len(want))
	}
	for i := range want {
		if config.Projects[i] != want[i] {
			t.Errorf("project %d = %+v, want %+v", i, config.Projects[i], want[i])
		}
	}
}

func TestLoadErrors(t *testing.T) {
	tests := []struct {
		name, key, value, wantErr string
	}{
		{"bad port", "PORT", "abc", "PORT"},
		{"port out of range", "PORT", "70000", "PORT"},
		{"bad log level", "LOG_LEVEL", "loud", "LOG_LEVEL"},
		{"data dir with query char", "DATA_DIR", "/srv/data?x", "DATA_DIR"},
		{"data dir with percent", "DATA_DIR", "/mnt/disk 100%", "DATA_DIR"},
		{"old format without id", "PROJECTS", "backend:a1b2c3d4e5f6a7b8", "expected id:name:public_key"},
		{"too many parts", "PROJECTS", "1:backend:a1b2c3d4e5f6a7b8:x", "expected id:name:public_key"},
		{"non-numeric id", "PROJECTS", "x:backend:a1b2c3d4e5f6a7b8", "invalid id"},
		{"zero id", "PROJECTS", "0:backend:a1b2c3d4e5f6a7b8", "invalid id"},
		{"bad name", "PROJECTS", "1:Back End:a1b2c3d4e5f6a7b8", "invalid name"},
		{"short key", "PROJECTS", "1:backend:abc", "16-64"},
		{"duplicate id", "PROJECTS", "1:backend:a1b2c3d4e5f6a7b8,1:frontend:0123456789abcdef", "duplicate project id"},
		{"duplicate name", "PROJECTS", "1:backend:a1b2c3d4e5f6a7b8,2:backend:0123456789abcdef", "duplicate project name"},
		{"duplicate key", "PROJECTS", "1:backend:a1b2c3d4e5f6a7b8,2:frontend:a1b2c3d4e5f6a7b8", "already used"},
	}
	for _, testCase := range tests {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := loadFrom(env(map[string]string{testCase.key: testCase.value}))
			if err == nil || !strings.Contains(err.Error(), testCase.wantErr) {
				t.Fatalf("got error %v, want it to contain %q", err, testCase.wantErr)
			}
		})
	}
}
