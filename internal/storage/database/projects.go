package database

import (
	"context"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
)

// deleteChunk is how many logs one batch of a project deletion removes, so
// a large project does not become one huge batch.
const deleteChunk = 5000

// DeleteProject deletes a project and all its data: the project itself
// first (its DSN stops working at once), then its issues with their events
// and status history, and its logs, in batches.
func (database *Store) DeleteProject(ctx context.Context, id int) error {
	if err := database.Store.DeleteProject(ctx, id); err != nil {
		return err
	}

	database.issuesMutex.RLock()

	var issueIDs []int64

	for issueID, issue := range database.table.byID {
		if issue.ProjectID == id {
			issueIDs = append(issueIDs, issueID)
		}
	}

	database.issuesMutex.RUnlock()

	for _, issueID := range issueIDs {
		if err := database.DeleteIssue(ctx, issueID); err != nil {
			return err
		}
	}

	for ctx.Err() == nil {
		deleted := 0

		err := database.update(func(writer *batchWriter) error {
			return database.logs.By("project", kv.U32(uint32(id))).Limit(deleteChunk).Each(database.db, func(entry kv.Entry[logRecord]) (bool, error) {
				database.logs.DeleteEntry(writer.batch, entry)

				deleted++

				return true, nil
			})
		})

		if err != nil || deleted < deleteChunk {
			return err
		}
	}

	return ctx.Err()
}
