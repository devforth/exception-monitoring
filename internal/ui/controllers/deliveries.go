package controllers

import (
	"net/http"
	"net/url"
	"slices"

	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/lists"
	"github.com/devforth/exception-monitoring/internal/ui/presenters"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
)

// The delivery log: every notification message with its status, attempts and
// last error, so a broken mail server or chat shows up without opening the
// database. Sent messages are kept for NOTIFY_KEEP_SENT, failed ones for
// NOTIFY_KEEP_FAILED.

var deliveryStatuses = []string{store.OutboxPending, store.OutboxFailed, store.OutboxSent}

func (controller *Controllers) DeliveriesPage(w http.ResponseWriter, r *http.Request) {
	connections, err := controller.connections.List(r.Context())

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	view := contracts.DeliveriesView{Labels: presenters.ConnectionLabels(connections), Statuses: deliveryStatuses}

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

	view.Connection, view.Status, view.Filter = query.Get("connection"), query.Get("status"), lists.NewFilterLinks(r, query)

	view.Connections = presenters.ConnectionOptions(connections, view.Connection)

	filter := store.OutboxFilter{Channel: view.Connection, Status: view.Status}
	total, err := controller.deliveries.Count(r.Context(), filter)

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	page, perPage := lists.ReadPaging(query)
	view.Pager = lists.NewPager(view.Filter, page, perPage, total, "delivery-list")
	filter.Limit, filter.Offset = perPage, (view.Pager.Page-1)*perPage

	records, err := controller.deliveries.List(r.Context(), filter)

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	view.Rows = presenters.DeliveryRows(records, view.Labels)

	if requests.IsHTMX(r) && r.Header.Get("HX-Target") == "delivery-list" {
		controller.views.RenderFragment(w, r, "deliveries", "delivery-list", view)

		return
	}

	controller.views.Render(w, r, http.StatusOK, "deliveries", view)
}
