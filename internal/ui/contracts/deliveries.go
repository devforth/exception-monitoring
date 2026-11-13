package contracts

import (
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/lists"
)

type DeliveryRow struct {
	store.OutboxRecord
	ConnectionID   string
	ConnectionName string
	Description    notify.Description
}

// Retrying: waiting for another attempt after a failed one.
func (row DeliveryRow) Retrying() bool {
	return row.Status == store.OutboxPending && row.Attempts > 0
}

type DeliveriesView struct {
	Connections []lists.Option // by id (Name) with the connection name in Label
	Labels      map[string]string
	Connection  string
	Status      string
	Statuses    []string
	Rows        []DeliveryRow
	Pager       lists.Pager
	Filter      lists.FilterLinks
}
