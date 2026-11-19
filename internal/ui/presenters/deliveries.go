package presenters

import (
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/storage/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/lists"
)

// ConnectionLabels names each connection by its id, as the outbox stores it.
func ConnectionLabels(connections []store.Connection) map[string]string {
	labels := map[string]string{}

	for _, connection := range connections {
		labels[connection.ID] = connection.Name
	}

	return labels
}

// ConnectionOptions are the choices of the connection filter, by id.
func ConnectionOptions(connections []store.Connection, selected string) []lists.Option {
	var options []lists.Option

	for _, connection := range connections {
		id := connection.ID
		options = append(options, lists.Option{Name: id, Selected: id == selected})
	}

	return options
}

// DeliveryRows describes each outbox message; labels are ConnectionLabels.
func DeliveryRows(records []store.OutboxRecord, labels map[string]string) []contracts.DeliveryRow {
	var rows []contracts.DeliveryRow

	for _, record := range records {
		rows = append(rows, contracts.DeliveryRow{
			OutboxRecord: record, ConnectionID: record.Channel, ConnectionName: labels[record.Channel],
			Description: notify.Describe(record.Kind, record.Payload),
		})
	}

	return rows
}
