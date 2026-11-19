// Package config loads server settings from environment variables.
package config

import (
	"cmp"
	"fmt"
	"log/slog"
	"net/netip"
	"net/url"
	"os"
	"regexp"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/logging"
)

// Project is a source of events (e.g. "backend", "frontend") with its own DSN.
// ID is the number at the end of the DSN and identifies the project's stored
// data, so it must never change once in use. Projects live in the database;
// the ones in PROJECTS are written there at every start with the same values.
type Project struct {
	ID        int
	Name      string
	PublicKey string
	// EventLevels and LogLevels are the project's own lists of levels to
	// record; nil uses Recording (an empty list records nothing).
	EventLevels *[]string
	LogLevels   *[]string
	// Color marks the project in lists and emails: "#rrggbb", set in the UI
	// or picked from ProjectPalette by id.
	Color string
}

// DSN is what a Sentry SDK is configured with: the server address (base,
// see access.PublicURL) with the project's key as the user and its ID as the
// path. The scheme stays: SDKs cannot parse a DSN without it.
func (project Project) DSN(base *url.URL) string {
	address := *base
	address.User = url.User(project.PublicKey)

	return address.JoinPath(strconv.Itoa(project.ID)).String()
}

// ProjectPalette gives projects distinct colors until one is chosen in the
// UI; all read well on light and dark backgrounds.
var ProjectPalette = []string{
	"#2563eb", "#db2777", "#16a34a", "#ea580c", "#7c3aed",
	"#0891b2", "#ca8a04", "#dc2626", "#4f46e5", "#0d9488",
}

// DefaultProjectColor is the palette color of a project id.
func DefaultProjectColor(id int) string {
	return ProjectPalette[(max(id, 1)-1)%len(ProjectPalette)]
}

var colorRe = regexp.MustCompile(`^#[0-9a-f]{6}$`)

// ValidColor: "#rrggbb" in lowercase, as the color input sends it. Colors go
// into CSS and email styles, so nothing else is accepted.
func ValidColor(color string) bool {
	return colorRe.MatchString(color)
}

// ProjectList gives the current projects. Projects can be added and changed
// in the UI while the server runs, so ingest, the API and the UI ask for the
// list on every use instead of keeping a copy (see package projects).
type ProjectList interface {
	List() Projects
}

// List returns the projects; a fixed list serves where a changing one is expected.
func (projects Projects) List() Projects {
	return projects
}

// ValidProjectName: lowercase letters, digits, '-' and '_' (used in URLs and
// filters), up to MaxProjectName.
func ValidProjectName(name string) bool {
	return len(name) <= MaxProjectName && projectNameRe.MatchString(name)
}

// Projects is the list from PROJECTS, in the order it was written.
type Projects []Project

// ByID returns the project with this ID. Stored data can belong to a project
// that was removed from PROJECTS, so callers handle ok == false.
func (projects Projects) ByID(id int) (Project, bool) {
	for _, project := range projects {
		if project.ID == id {
			return project, true
		}
	}

	return Project{}, false
}

// Find returns the project given as an ID ("1") or a name ("backend"), the
// two ways a project is referred to in URLs.
func (projects Projects) Find(idOrName string) (Project, bool) {
	for _, project := range projects {
		if strconv.Itoa(project.ID) == idOrName || project.Name == idOrName {
			return project, true
		}
	}

	return Project{}, false
}

type Config struct {
	// AppName (APP_NAME) names this server in the UI, emails and to agents.
	AppName  string
	Port     int
	DataDir  string
	LogLevel slog.Level
	// LogFormat (LOG_FORMAT): auto (pretty in a terminal, text otherwise),
	// pretty, text or json; see package logging.
	LogFormat string
	Projects  Projects
	// AllowedOrigins lists the browser origins allowed to send events (CORS).
	// ["*"] allows any origin. Requests without an Origin header (backend SDKs) are always allowed.
	AllowedOrigins []string
	// RateLimitPerMinute caps envelopes per project per minute. 0 disables the limit.
	RateLimitPerMinute int
	// APIToken authorizes REST API requests (Authorization: Bearer <token>).
	// Empty means the API is closed.
	APIToken string
	// AllowedIPs restricts the API and UI (not ingest or /health) to these
	// networks. Empty means any address.
	AllowedIPs []netip.Prefix
	// TrustedProxies are reverse proxies whose X-Forwarded-For header is used
	// to find the client address. Empty means the header is ignored.
	TrustedProxies []netip.Prefix
	// AdminUsername and AdminPassword create the superadmin at the first
	// start (while there is none); later they are not used.
	// An empty password means the UI is closed.
	AdminUsername string
	AdminPassword string
	// SessionLifetime (SESSION_LIFETIME) is how long a UI login lasts; it
	// is not extended by use.
	SessionLifetime time.Duration
	// Notifications: where to send alerts about new and regressed issues.
	Notifications Notifications
	// Retention: how much data is kept (MAX_DATABASE_SIZE).
	Retention Retention
	// Recording: which event and log levels are stored by default.
	Recording Recording
}

// Defaults used when the corresponding environment variable is not set.
const (
	DefaultPort               = 8000
	DefaultDataDir            = "./data"
	DefaultLogLevel           = slog.LevelInfo
	DefaultAllowedOrigins     = "*"
	DefaultRateLimitPerMinute = 1000
	DefaultMemoryLimit        = 200 << 20 // GOMEMLIMIT, the Go runtime's soft limit

	DefaultAdminUsername = "admin"

	DefaultSessionLifetime = 24 * time.Hour

	// MinAPITokenLength keeps tokens out of brute-force range.
	MinAPITokenLength = 32
	// MinPasswordLength applies to every user's password: logins are rate
	// limited, but the UI is often public.
	MinPasswordLength = 12
	// Users: the username (letters, digits and . _ @ -, UsernamePattern) and
	// the display name; passwords up to MaxPasswordLength.
	MaxUsername       = 64
	MaxUserName       = 100
	MaxPasswordLength = 256
	// UsernamePattern is also the pattern attribute of the form, hence the
	// escaped '-'; Go's regexp accepts it too.
	UsernamePattern = `[A-Za-z0-9._@\-]+`
	// MaxConnectionName is the longest name of a notification connection;
	// VerificationCodeDigits the length of an email confirmation code.
	MaxConnectionName      = 100
	VerificationCodeDigits = 6
	// MaxProjectName is the longest project name.
	MaxProjectName = 64
	// ProjectNamePattern: lowercase letters, digits, '-' and '_', starting
	// with a letter or digit. Also the pattern attribute of the project form,
	// hence the escaped '-'.
	ProjectNamePattern = `[a-z0-9][a-z0-9_\-]*`
)

var (
	projectNameRe = regexp.MustCompile(`^` + ProjectNamePattern + `$`)
	publicKeyRe   = regexp.MustCompile(`^[A-Za-z0-9]{16,64}$`)
	originRe      = regexp.MustCompile(`^https?://([a-z0-9.-]+|\[[0-9a-f:]+\])(:[0-9]{1,5})?$`)
)

// Load reads the configuration from the process environment.
func Load() (*Config, error) {
	return loadFrom(os.Getenv)
}

// LimitMemory sets DefaultMemoryLimit unless GOMEMLIMIT is set; the load tests call it to measure what the server does.
func LimitMemory() {
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(DefaultMemoryLimit)
	}
}

func loadFrom(getenv func(string) string) (*Config, error) {
	config := &Config{
		Port:               DefaultPort,
		DataDir:            DefaultDataDir,
		LogLevel:           DefaultLogLevel,
		AllowedOrigins:     []string{DefaultAllowedOrigins},
		RateLimitPerMinute: DefaultRateLimitPerMinute,
		AdminUsername:      DefaultAdminUsername,
		SessionLifetime:    DefaultSessionLifetime,
	}

	var err error

	if config.AppName, err = parseAppName(getenv("APP_NAME")); err != nil {
		return nil, err
	}

	if value := getenv("PORT"); value != "" {
		port, err := strconv.Atoi(value)

		if err != nil || port < 1 || port > 65535 {
			return nil, fmt.Errorf("PORT: invalid port %q", value)
		}

		config.Port = port
	}

	if value := getenv("DATA_DIR"); value != "" {
		config.DataDir = value
	}

	if value := getenv("LOG_LEVEL"); value != "" {
		if err := config.LogLevel.UnmarshalText([]byte(value)); err != nil {
			return nil, fmt.Errorf("LOG_LEVEL: %w", err)
		}
	}

	config.LogFormat = cmp.Or(strings.ToLower(getenv("LOG_FORMAT")), logging.FormatAuto)

	if !slices.Contains(logging.Formats, config.LogFormat) {
		return nil, fmt.Errorf("LOG_FORMAT: %q is not one of %s", config.LogFormat, strings.Join(logging.Formats, ", "))
	}

	if value := getenv("ALLOWED_ORIGINS"); value != "" {
		origins, err := parseOrigins(value)

		if err != nil {
			return nil, fmt.Errorf("ALLOWED_ORIGINS: %w", err)
		}

		config.AllowedOrigins = origins
	}

	if value := getenv("RATE_LIMIT_PER_MINUTE"); value != "" {
		limit, err := strconv.Atoi(value)

		if err != nil || limit < 0 {
			return nil, fmt.Errorf("RATE_LIMIT_PER_MINUTE: invalid value %q (0 or a positive integer)", value)
		}

		config.RateLimitPerMinute = limit
	}

	if value := getenv("API_TOKEN"); value != "" {
		if len(value) < MinAPITokenLength {
			return nil, fmt.Errorf("API_TOKEN: must be at least %d characters (e.g. openssl rand -hex 32)", MinAPITokenLength)
		}

		config.APIToken = value
	}

	if value := getenv("ADMIN_USERNAME"); value != "" {
		config.AdminUsername = value
	}

	if value := getenv("ADMIN_PASSWORD"); value != "" {
		if len([]rune(value)) < MinPasswordLength {
			return nil, fmt.Errorf("ADMIN_PASSWORD: must be at least %d characters", MinPasswordLength)
		}

		config.AdminPassword = value
	}

	if value := strings.TrimSpace(getenv("SESSION_LIFETIME")); value != "" {
		if config.SessionLifetime, err = parseDuration(value); err != nil {
			return nil, fmt.Errorf("SESSION_LIFETIME: %w", err)
		}
	}

	notifications, err := parseNotifications(getenv)

	if err != nil {
		return nil, err
	}

	config.Notifications = notifications

	recording, err := parseRecording(getenv)

	if err != nil {
		return nil, err
	}

	config.Recording = recording

	retention, err := parseRetention(getenv)

	if err != nil {
		return nil, err
	}

	config.Retention = retention

	allowedIPs, err := parseNetworks(getenv("ALLOWED_IPS"))

	if err != nil {
		return nil, fmt.Errorf("ALLOWED_IPS: %w", err)
	}

	config.AllowedIPs = allowedIPs

	trustedProxies, err := parseNetworks(getenv("TRUSTED_PROXIES"))

	if err != nil {
		return nil, fmt.Errorf("TRUSTED_PROXIES: %w", err)
	}

	config.TrustedProxies = trustedProxies

	projects, err := parseProjects(getenv("PROJECTS"))

	if err != nil {
		return nil, fmt.Errorf("PROJECTS: %w", err)
	}

	config.Projects = projects

	return config, nil
}

// parseProjects parses "id:name:key,id:name:key". Empty input means no projects.
func parseProjects(raw string) (Projects, error) {
	raw = strings.TrimSpace(raw)

	if raw == "" {
		return nil, nil
	}

	var projects Projects

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

// parseOrigins parses "*" or "https://app.example.com,http://localhost:3000".
// Origins are compared exactly, so a path or trailing slash is an error.
func parseOrigins(raw string) ([]string, error) {
	var origins []string

	for _, entry := range strings.Split(raw, ",") {
		origin := strings.ToLower(strings.TrimSpace(entry))

		if origin == "" {
			continue
		}

		if origin != "*" && !originRe.MatchString(origin) {
			return nil, fmt.Errorf("invalid origin %q (expected scheme://host[:port], e.g. https://app.example.com)", entry)
		}

		origins = append(origins, origin)
	}

	if len(origins) == 0 {
		return nil, fmt.Errorf("no origins in %q", raw)
	}

	return origins, nil
}

// parseNetworks parses "10.0.0.0/8, 203.0.113.7, ::1". A plain address means
// just that address (/32 or /128).
func parseNetworks(raw string) ([]netip.Prefix, error) {
	var networks []netip.Prefix

	for _, entry := range strings.Split(raw, ",") {
		entry = strings.TrimSpace(entry)

		if entry == "" {
			continue
		}

		if strings.Contains(entry, "/") {
			network, err := netip.ParsePrefix(entry)

			if err != nil {
				return nil, fmt.Errorf("invalid network %q", entry)
			}

			networks = append(networks, network.Masked())

			continue
		}

		address, err := netip.ParseAddr(entry)

		if err != nil {
			return nil, fmt.Errorf("invalid address %q", entry)
		}

		networks = append(networks, netip.PrefixFrom(address.Unmap(), address.Unmap().BitLen()))
	}

	return networks, nil
}
