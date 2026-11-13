package services

import (
	"context"

	"github.com/devforth/exception-monitoring/internal/store"
)

// Deliveries reads the notification outbox for the delivery log.
type Deliveries struct {
	store *store.Store
}

func NewDeliveries(database *store.Store) *Deliveries {
	return &Deliveries{store: database}
}

// Count counts the messages that match filter (its Limit and Offset are ignored).
func (service *Deliveries) Count(ctx context.Context, filter store.OutboxFilter) (int, error) {
	return service.store.CountOutbox(ctx, filter)
}

// List returns one page of the messages that match filter, newest first.
func (service *Deliveries) List(ctx context.Context, filter store.OutboxFilter) ([]store.OutboxRecord, error) {
	return service.store.ListOutbox(ctx, filter)
}
