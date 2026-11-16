package store

import (
	"context"
	"fmt"
	"strings"
	"time"
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

// issueFields are the columns scanIssue reads.
const issueFields = `id, project_id, title, culprit, level, status, event_count, first_seen, last_seen, resolved_at`

func issueBatchArgs(ids []int64) (string, []any) {
	args := make([]any, len(ids))

	for index, id := range ids {
		args[index] = id
	}

	return strings.TrimSuffix(strings.Repeat("?,", len(ids)), ","), args
}

// GetIssues returns summaries in one query, without loading event payloads.
// Missing IDs are omitted; callers can report them alongside the found issues.
func (store *Store) GetIssues(ctx context.Context, issueIDs []int64) ([]Issue, error) {
	ids, err := NormalizeIssueIDs(issueIDs)

	if err != nil {
		return nil, err
	}

	placeholders, args := issueBatchArgs(ids)
	rows, err := store.DB.QueryContext(ctx, `SELECT `+issueFields+` FROM issues WHERE id IN (`+placeholders+`) ORDER BY id`, args...)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	issues := make([]Issue, 0, len(ids))

	for rows.Next() {
		issue, err := scanIssue(rows)

		if err != nil {
			return nil, err
		}

		issues = append(issues, issue)
	}

	return issues, rows.Err()
}

// SetIssuesStatus applies one status to a batch atomically, recording actor
// in the history of every issue whose status changes. Missing IDs
// or invalid input leave every issue unchanged (ErrNotFound when one is
// missing). Returned rows need no re-fetch. resolved_at keeps the first
// resolve time while an issue stays resolved; any other status clears it.
func (store *Store) SetIssuesStatus(ctx context.Context, issueIDs []int64, status string, actor Actor) ([]Issue, error) {
	ids, err := NormalizeIssueIDs(issueIDs)

	if err != nil {
		return nil, err
	}

	switch status {
	case StatusResolved, StatusIgnored, StatusUnresolved:
	default:
		return nil, fmt.Errorf("invalid status %q", status)
	}

	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return nil, err
	}

	defer transaction.Rollback()

	placeholders, args := issueBatchArgs(ids)
	now := time.Now()

	if err := recordStatusChanges(ctx, transaction, placeholders, args, status, actor, "", now); err != nil {
		return nil, err
	}

	args = append([]any{status, status, StatusResolved, formatTime(now)}, args...)
	rows, err := transaction.QueryContext(ctx, `UPDATE issues SET status = ?,
		resolved_at = CASE WHEN ? = ? THEN COALESCE(resolved_at, ?) ELSE NULL END
		WHERE id IN (`+placeholders+`) RETURNING `+issueFields, args...)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	issues := make([]Issue, 0, len(ids))

	for rows.Next() {
		issue, err := scanIssue(rows)

		if err != nil {
			return nil, err
		}

		issues = append(issues, issue)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	if err := rows.Close(); err != nil {
		return nil, err
	}

	if len(issues) != len(ids) {
		return nil, ErrNotFound
	}

	if err := transaction.Commit(); err != nil {
		return nil, err
	}

	return issues, nil
}
