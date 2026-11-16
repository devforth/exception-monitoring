package services

import (
	"context"

	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Logs reads the logs list.
type Logs struct {
	store *store.Store
}

func NewLogs(database *store.Store) *Logs {
	return &Logs{store: database}
}

// Count counts the logs that match filter (its Limit and Offset are ignored).
func (service *Logs) Count(ctx context.Context, filter store.LogFilter) (int, error) {
	return service.store.CountLogs(ctx, filter)
}

// List returns one page of the logs that match filter, newest first.
func (service *Logs) List(ctx context.Context, filter store.LogFilter) ([]store.LogRecord, error) {
	return service.store.ListLogs(ctx, filter)
}

// Issues gives the issue each log was written with, by log id: the logs
// list links to it.
func (service *Logs) Issues(ctx context.Context, logs []store.LogRecord) (map[int64]store.LogIssue, error) {
	return service.store.IssuesForLogs(ctx, logs)
}
