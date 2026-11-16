// Package text holds small string helpers shared by the services and the UI.
package text

import "strings"

// TruncateRunes cuts text to at most limit characters (not bytes).
func TruncateRunes(text string, limit int) string {
	if runes := []rune(text); len(runes) > limit {
		return string(runes[:limit])
	}

	return text
}

// EmailLocalPart is the part of an address before "@": the default name of
// an email connection.
func EmailLocalPart(address string) string {
	local, _, _ := strings.Cut(address, "@")

	return local
}
