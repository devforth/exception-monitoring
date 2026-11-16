package endpoints

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
)

// Lists return DefaultLimit rows unless limit says otherwise, MaxLimit at most.
const (
	DefaultLimit = 20
	MaxLimit     = 100
)

// Page is a list with the cursor of the next page; no cursor on the last one.
type Page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

func parseLimit(limit int) (int, error) {
	if limit == 0 {
		return DefaultLimit, nil
	}

	if limit < 1 || limit > MaxLimit {
		return 0, Invalid("limit must be between 1 and %d", MaxLimit)
	}

	return limit, nil
}

// Cursors are plain numbers: an offset for issues and projects (sorted by
// fields that change), the last ID seen for events and logs (newest first).
func parseCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}

	value, err := strconv.ParseInt(cursor, 10, 64)

	if err != nil || value < 0 {
		return 0, Invalid("invalid cursor: pass next_cursor of the previous page")
	}

	return value, nil
}

// projectIDs resolves a project given by name or ID as a filter: none when empty.
func projectIDs(projects config.Projects, nameOrID string) ([]int, error) {
	if nameOrID = strings.TrimSpace(nameOrID); nameOrID == "" {
		return nil, nil
	}

	project, ok := projects.Find(nameOrID)

	if !ok {
		return nil, Invalid("unknown project %q (see list_projects)", nameOrID)
	}

	return []int{project.ID}, nil
}

// levels checks a level filter against the known levels: none when empty.
func levels(level string, known []string) ([]string, error) {
	if level == "" {
		return nil, nil
	}

	if !slices.Contains(known, level) {
		return nil, Invalid("level must be one of %s", strings.Join(known, ", "))
	}

	return []string{level}, nil
}

func parseTime(name, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}

	parsed, err := time.Parse(time.RFC3339Nano, value)

	if err != nil {
		return time.Time{}, Invalid("%s must be an RFC 3339 time, e.g. 2026-11-11T10:00:00Z", name)
	}

	return parsed, nil
}
