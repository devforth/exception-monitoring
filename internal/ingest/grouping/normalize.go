package grouping

import "strings"

// NormalizeMessage replaces the parts of a message that differ between
// occurrences of the same problem: UUIDs, hex IDs (8+ chars) and numbers.
// Three passes in that order, each written out instead of a regexp (it runs
// on every log line); normalize_test.go checks they give what the regexps
// `[0-9a-fA-F]{8}-([0-9a-fA-F]{4}-){3}[0-9a-fA-F]{12}`, `\b(0x)?[0-9a-fA-F]{8,}\b`
// and `\d+` give, since a change would regroup issues.
func NormalizeMessage(message string) string {
	return replaceNumbers(replaceHex(replaceUUIDs(message)))
}

func isHex(char byte) bool {
	return '0' <= char && char <= '9' || 'a' <= char && char <= 'f' || 'A' <= char && char <= 'F'
}

// isWordChar is a word character of regexp's \b: ASCII letters, digits and _.
func isWordChar(char byte) bool {
	return '0' <= char && char <= '9' || 'a' <= char && char <= 'z' || 'A' <= char && char <= 'Z' || char == '_'
}

// uuidGroups are the hex runs of a UUID, separated by dashes.
var uuidGroups = [...]int{8, 4, 4, 4, 12}

const uuidLength = 36

func isUUID(text string) bool {
	position := 0

	for index, size := range uuidGroups {
		if index > 0 {
			if text[position] != '-' {
				return false
			}

			position++
		}

		for end := position + size; position < end; position++ {
			if !isHex(text[position]) {
				return false
			}
		}
	}

	return true
}

func replaceUUIDs(text string) string {
	if strings.Count(text, "-") < 4 {
		return text
	}

	var output strings.Builder

	last := 0

	for index := 0; index+uuidLength <= len(text); {
		if isUUID(text[index : index+uuidLength]) {
			output.WriteString(text[last:index])
			output.WriteString("<uuid>")

			index += uuidLength
			last = index

			continue
		}

		index++
	}

	if last == 0 {
		return text
	}

	output.WriteString(text[last:])

	return output.String()
}

// replaceHex replaces whole words of 8+ hex digits, optionally after 0x.
func replaceHex(text string) string {
	var output strings.Builder

	last := 0

	for index := 0; index < len(text); {
		if index > 0 && isWordChar(text[index-1]) || !isHex(text[index]) {
			index++

			continue
		}

		start := index
		end := index

		if text[index] == '0' && index+1 < len(text) && text[index+1] == 'x' {
			end = index + 2
		}

		run := end

		for run < len(text) && isHex(text[run]) {
			run++
		}

		matched := run-end >= 8 && (run == len(text) || !isWordChar(text[run]))

		if !matched && end != index {
			// Without the 0x prefix the run is "0" followed by x: too short.
			run = index

			for run < len(text) && isHex(text[run]) {
				run++
			}

			matched = run-index >= 8 && (run == len(text) || !isWordChar(text[run]))
		}

		if matched {
			output.WriteString(text[last:start])
			output.WriteString("<hex>")

			last = run
			index = run

			continue
		}

		index++
	}

	if last == 0 {
		return text
	}

	output.WriteString(text[last:])

	return output.String()
}

func replaceNumbers(text string) string {
	var output strings.Builder

	last := 0

	for index := 0; index < len(text); {
		if text[index] < '0' || text[index] > '9' {
			index++

			continue
		}

		end := index

		for end < len(text) && '0' <= text[end] && text[end] <= '9' {
			end++
		}

		output.WriteString(text[last:index])
		output.WriteString("<n>")

		last, index = end, end
	}

	if last == 0 {
		return text
	}

	output.WriteString(text[last:])

	return output.String()
}
