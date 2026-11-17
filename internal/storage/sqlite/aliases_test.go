package sqlite

import (
	"slices"
	"testing"

	domain "github.com/devforth/exception-monitoring/internal/storage/store"
)

// Every sort store accepts has its SQL here, and nothing more.
func TestIssueSortsMatchTheStore(t *testing.T) {
	var keys []string

	for key := range issueSorts {
		keys = append(keys, key)
	}

	sorts := slices.Clone(domain.IssueSorts)

	slices.Sort(keys)
	slices.Sort(sorts)

	if !slices.Equal(keys, sorts) {
		t.Errorf("sqlite sorts %v, store sorts %v", keys, sorts)
	}
}
