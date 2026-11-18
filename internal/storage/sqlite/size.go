package sqlite

import (
	"context"
	"fmt"

	domain "github.com/devforth/exception-monitoring/internal/storage/store"
)

// DatabaseSize returns the bytes used by data (the file size minus free pages).
func (store *Store) DatabaseSize(ctx context.Context) (int64, error) {
	var pageCount, freePages, pageSize int64

	for query, target := range map[string]*int64{
		`PRAGMA page_count`:     &pageCount,
		`PRAGMA freelist_count`: &freePages,
		`PRAGMA page_size`:      &pageSize,
	} {
		if err := store.DB.QueryRowContext(ctx, query).Scan(target); err != nil {
			return 0, fmt.Errorf("%s: %w", query, err)
		}
	}

	return (pageCount - freePages) * pageSize, nil
}

// CleanupHistory is empty: the cleanup by size runs on the key-value store only.
func (store *Store) CleanupHistory(context.Context, int) ([]domain.CleanupRun, error) {
	return nil, nil
}
