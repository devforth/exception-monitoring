package config

import (
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func env(values map[string]string) func(string) string {
	return func(name string) string { return values[name] }
}

func TestLoadDefaults(t *testing.T) {
	config, err := loadFrom(env(nil))

	if err != nil {
		t.Fatal(err)
	}

	if config.Port != DefaultPort || config.DataDir != DefaultDataDir || config.LogLevel != DefaultLogLevel || config.Projects != nil || config.APIToken != "" || config.AllowedIPs != nil ||
		len(config.AllowedOrigins) != 1 || config.AllowedOrigins[0] != DefaultAllowedOrigins || config.RateLimitPerMinute != DefaultRateLimitPerMinute ||
		config.AdminUsername != DefaultAdminUsername || config.AdminPassword != "" || config.SessionLifetime != 24*time.Hour {
		t.Fatalf("unexpected defaults: %+v", config)
	}
}

func TestLoadValues(t *testing.T) {
	config, err := loadFrom(env(map[string]string{
		"PORT":                  "9000",
		"DATA_DIR":              "/data",
		"LOG_LEVEL":             "debug",
		"ALLOWED_ORIGINS":       "https://App.example.com, http://localhost:3500,http://[::1]:8080",
		"RATE_LIMIT_PER_MINUTE": "0",
		"API_TOKEN":             "0123456789abcdef0123456789abcdef",
		"ALLOWED_IPS":           "10.0.0.0/8, 203.0.113.7,::1",
		"TRUSTED_PROXIES":       "172.17.0.1",
		"ADMIN_USERNAME":        "root",
		"ADMIN_PASSWORD":        "root-password",
		"SESSION_LIFETIME":      "12h",
		// IDs are explicit, so they don't have to follow list order or be contiguous.
		"PROJECTS": "3:frontend:0123456789abcdef0123, 1:backend:a1b2c3d4e5f6a7b8",
	}))

	if err != nil {
		t.Fatal(err)
	}

	if config.Port != 9000 || config.DataDir != "/data" || config.LogLevel != slog.LevelDebug || config.SessionLifetime != 12*time.Hour {
		t.Fatalf("unexpected config: %+v", config)
	}

	if strings.Join(config.AllowedOrigins, " ") != "https://app.example.com http://localhost:3500 http://[::1]:8080" {
		t.Fatalf("origins = %q", config.AllowedOrigins)
	}

	if config.RateLimitPerMinute != 0 {
		t.Fatalf("rate limit = %d, want 0 (disabled)", config.RateLimitPerMinute)
	}

	if config.APIToken != "0123456789abcdef0123456789abcdef" {
		t.Fatalf("api token = %q", config.APIToken)
	}

	if config.AdminUsername != "root" || config.AdminPassword != "root-password" {
		t.Fatalf("admin = %q / %q", config.AdminUsername, config.AdminPassword)
	}

	if fmt.Sprint(config.AllowedIPs) != "[10.0.0.0/8 203.0.113.7/32 ::1/128]" || fmt.Sprint(config.TrustedProxies) != "[172.17.0.1/32]" {
		t.Fatalf("allowed=%v trusted=%v", config.AllowedIPs, config.TrustedProxies)
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
		{"bad log format", "LOG_FORMAT", "fancy", "LOG_FORMAT"},
		{"old format without id", "PROJECTS", "backend:a1b2c3d4e5f6a7b8", "expected id:name:public_key"},
		{"too many parts", "PROJECTS", "1:backend:a1b2c3d4e5f6a7b8:x", "expected id:name:public_key"},
		{"non-numeric id", "PROJECTS", "x:backend:a1b2c3d4e5f6a7b8", "invalid id"},
		{"zero id", "PROJECTS", "0:backend:a1b2c3d4e5f6a7b8", "invalid id"},
		{"bad name", "PROJECTS", "1:Back End:a1b2c3d4e5f6a7b8", "invalid name"},
		{"short key", "PROJECTS", "1:backend:abc", "16-64"},
		{"duplicate id", "PROJECTS", "1:backend:a1b2c3d4e5f6a7b8,1:frontend:0123456789abcdef", "duplicate project id"},
		{"duplicate name", "PROJECTS", "1:backend:a1b2c3d4e5f6a7b8,2:backend:0123456789abcdef", "duplicate project name"},
		{"origin with path", "ALLOWED_ORIGINS", "https://app.example.com/", "invalid origin"},
		{"origin without scheme", "ALLOWED_ORIGINS", "app.example.com", "invalid origin"},
		{"only commas in origins", "ALLOWED_ORIGINS", ",,", "no origins"},
		{"negative rate limit", "RATE_LIMIT_PER_MINUTE", "-1", "RATE_LIMIT_PER_MINUTE"},
		{"short api token", "API_TOKEN", "secret", "at least"},
		{"short admin password", "ADMIN_PASSWORD", "short-pass", "at least 12"},
		{"bad session lifetime", "SESSION_LIFETIME", "forever", "SESSION_LIFETIME"},
		{"zero session lifetime", "SESSION_LIFETIME", "0", "SESSION_LIFETIME"},
		{"bad allowed ip", "ALLOWED_IPS", "10.0.0.300", "invalid address"},
		{"bad network", "TRUSTED_PROXIES", "10.0.0.0/40", "invalid network"},
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

func TestProjectsLookup(t *testing.T) {
	projects := Projects{{ID: 3, Name: "frontend"}, {ID: 1, Name: "backend"}}

	for _, idOrName := range []string{"1", "backend"} {
		if project, ok := projects.Find(idOrName); !ok || project.ID != 1 {
			t.Errorf("Find(%q) = %+v, %v", idOrName, project, ok)
		}
	}

	if _, ok := projects.Find("billing"); ok {
		t.Error("Find of an unknown project must fail")
	}

	if project, ok := projects.ByID(3); !ok || project.Name != "frontend" {
		t.Errorf("ByID(3) = %+v, %v", project, ok)
	}

	if _, ok := projects.ByID(2); ok {
		t.Error("ByID of an unknown project must fail")
	}
}

// The ingest queue gets a fifth of the size limit, of the default one when the limit is off.
func TestMaxInboxBytes(t *testing.T) {
	if got := MaxInboxBytes(1000); got != 200 {
		t.Errorf("of 1000: %d", got)
	}

	if got := MaxInboxBytes(0); got != DefaultMaxDatabaseBytes/5 {
		t.Errorf("with no limit: %d", got)
	}
}
