package config

import (
	"fmt"
	"strings"
	"unicode/utf8"
)

// DefaultAppName names the server when APP_NAME is not set.
const DefaultAppName = "Exception Monitoring"

const maxAppName = 60

// parseAppName reads APP_NAME: the name shown in the sidebar, page titles,
// emails and to coding agents.
func parseAppName(value string) (string, error) {
	name := strings.Join(strings.Fields(value), " ")

	if name == "" {
		return DefaultAppName, nil
	}

	if utf8.RuneCountInString(name) > maxAppName {
		return "", fmt.Errorf("APP_NAME: at most %d characters", maxAppName)
	}

	return name, nil
}

// MCPServerName is the name agents register the MCP server under (`claude
// mcp add <name> ...`): agents accept letters, digits, "-" and "_", so the
// app name is lowercased with every other run of characters, spaces
// included, turned into one "-" ("Acme Errors" → "acme-errors"). A name with
// no Latin letters or digits falls back to "exception-monitoring".
func MCPServerName(appName string) string {
	var name strings.Builder

	for _, char := range strings.ToLower(appName) {
		switch {
		case char >= 'a' && char <= 'z', char >= '0' && char <= '9', char == '_':
			name.WriteRune(char)
		case name.Len() > 0 && !strings.HasSuffix(name.String(), "-"):
			name.WriteRune('-')
		}
	}

	if slug := strings.Trim(name.String(), "-"); slug != "" {
		return slug
	}

	return MCPServerName(DefaultAppName)
}
