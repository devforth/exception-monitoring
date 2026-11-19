package store

import (
	"fmt"
)

const MaxIssueBatch = 100

// NormalizeIssueIDs validates the entire batch before any query or mutation.
// Duplicate IDs are handled once, preserving the supplied order.
func NormalizeIssueIDs(ids []string) ([]string, error) {
	if len(ids) == 0 || len(ids) > MaxIssueBatch {
		return nil, fmt.Errorf("issue_ids must contain between 1 and %d IDs", MaxIssueBatch)
	}

	unique := make([]string, 0, len(ids))
	seen := make(map[string]bool, len(ids))

	for _, id := range ids {
		if !ValidID(id) {
			return nil, fmt.Errorf("issue IDs are %d lowercase letters or digits, not %q", IDLength, id)
		}

		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}

	return unique, nil
}
