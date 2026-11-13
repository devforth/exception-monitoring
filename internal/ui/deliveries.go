package ui

import (
	"net/http"
	"net/url"
	"slices"
	"strconv"

	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/store"
)

// The delivery log: every notification message with its status, attempts and
// last error, so a broken mail server or chat shows up without opening the
// database. Sent messages are kept for NOTIFY_KEEP_SENT, failed ones for
// NOTIFY_KEEP_FAILED.

var deliveryStatuses = []string{store.OutboxPending, store.OutboxFailed, store.OutboxSent}

type deliveryRow struct {
	store.OutboxRecord
	ConnectionID   string
	ConnectionName string
	Description    notify.Description
}

// Retrying: waiting for another attempt after a failed one.
func (row deliveryRow) Retrying() bool {
	return row.Status == store.OutboxPending && row.Attempts > 0
}

type deliveriesView struct {
	Connections []option // by id (Name) with the connection name in Label
	Labels      map[string]string
	Connection  string
	Status      string
	Statuses    []string
	Rows        []deliveryRow
	Pager       pager
	Filter      filterLinks
}

func (handler *Handler) deliveriesPage(w http.ResponseWriter, r *http.Request) {
	connections, err := handler.store.ListConnections(r.Context())

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	view := deliveriesView{Labels: map[string]string{}, Statuses: deliveryStatuses}

	for _, connection := range connections {
		view.Labels[strconv.FormatInt(connection.ID, 10)] = connection.Name
	}

	// Only known values: a hand-edited URL filters by nothing it does not show.
	raw, query := r.URL.Query(), url.Values{}

	if id := raw.Get("connection"); view.Labels[id] != "" {
		query.Set("connection", id)
	}

	if status := raw.Get("status"); slices.Contains(deliveryStatuses, status) {
		query.Set("status", status)
	}

	for _, key := range []string{"page", "per_page"} {
		if value := raw.Get(key); value != "" {
			query.Set(key, value)
		}
	}

	view.Connection, view.Status, view.Filter = query.Get("connection"), query.Get("status"), newFilterLinks(r, query)

	for _, connection := range connections {
		id := strconv.FormatInt(connection.ID, 10)
		view.Connections = append(view.Connections, option{Name: id, Selected: id == view.Connection})
	}

	filter := store.OutboxFilter{Channel: view.Connection, Status: view.Status}
	total, err := handler.store.CountOutbox(r.Context(), filter)

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	page, perPage := readPaging(query)
	view.Pager = newPager(view.Filter, page, perPage, total, "delivery-list")
	filter.Limit, filter.Offset = perPage, (view.Pager.Page-1)*perPage

	records, err := handler.store.ListOutbox(r.Context(), filter)

	if err != nil {
		handler.serverError(w, r, err)

		return
	}

	for _, record := range records {
		view.Rows = append(view.Rows, deliveryRow{
			OutboxRecord: record, ConnectionID: record.Channel, ConnectionName: view.Labels[record.Channel],
			Description: notify.Describe(record.Kind, record.Payload),
		})
	}

	if isHTMX(r) && r.Header.Get("HX-Target") == "delivery-list" {
		handler.renderFragment(w, r, "deliveries", "delivery-list", view)

		return
	}

	handler.render(w, r, http.StatusOK, "deliveries", view)
}
