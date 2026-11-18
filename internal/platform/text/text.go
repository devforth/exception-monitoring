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

// Shorten cuts text to at most limit characters (not bytes), ending with
// "…" when it was cut.
func Shorten(text string, limit int) string {
	runes := []rune(text)

	if len(runes) <= limit {
		return text
	}

	return string(runes[:limit-1]) + "…"
}

// ContainsFold reports whether text contains part, ignoring the case of
// ASCII letters only, as SQLite's LIKE does.
func ContainsFold(text, part string) bool {
	if part == "" {
		return true
	}

	first := lowerASCII(part[0])

	for index := 0; index+len(part) <= len(text); index++ {
		if lowerASCII(text[index]) == first && equalFoldASCII(text[index:index+len(part)], part) {
			return true
		}
	}

	return false
}

func lowerASCII(char byte) byte {
	if 'A' <= char && char <= 'Z' {
		return char + 'a' - 'A'
	}

	return char
}

func equalFoldASCII(a, b string) bool {
	for index := range len(a) {
		if lowerASCII(a[index]) != lowerASCII(b[index]) {
			return false
		}
	}

	return true
}

// EmailLocalPart is the part of an address before "@": the default name of
// an email connection.
func EmailLocalPart(address string) string {
	local, _, _ := strings.Cut(address, "@")

	return local
}
