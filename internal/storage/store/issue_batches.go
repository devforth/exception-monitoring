package store

import (
	"fmt"
)

const MaxIssueBatch = 100

// NormalizeIssueIDs validates the entire batch before any query or mutation.
// Duplicate IDs are handled once, preserving the supplied order.
func NormalizeIssueIDs(ids []int64) ([]int64, error) {
	if len(ids) == 0 || len(ids) > MaxIssueBatch {
		return nil, fmt.Errorf("issue_ids must contain between 1 and %d IDs", MaxIssueBatch)
	}

	unique := make([]int64, 0, len(ids))
	seen := make(map[int64]bool, len(ids))

	for _, id := range ids {
		if id <= 0 {
			return nil, fmt.Errorf("issue IDs must be positive")
		}

		if !seen[id] {
			seen[id] = true
			unique = append(unique, id)
		}
	}

	return unique, nil
}
