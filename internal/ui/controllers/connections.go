package controllers

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
)

// Notification connections: where alerts go (an email address, a Telegram
// chat, a Slack webhook) and which projects and levels each one receives.
// An email address is confirmed first (see email.go).

var notices = map[string]string{
	"saved":       "Saved.",
	"created":     "Connection added.",
	"test-sent":   "Test message sent.",
	"deleted":     "Connection deleted.",
	"activated":   "Connection activated.",
	"deactivated": "Connection deactivated.",
}

func (controller *Controllers) ConnectionsPage(w http.ResponseWriter, r *http.Request) {
	connections, err := controller.connections.List(r.Context())

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	health, err := controller.connections.DeliveryHealth(r.Context())

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	view := controller.connectionViews.List(connections, health, notices[r.URL.Query().Get("notice")])

	controller.views.Render(w, r, http.StatusOK, "notifications", view)
}

func (controller *Controllers) NewConnectionPage(w http.ResponseWriter, r *http.Request) {
	connection := controller.connectionViews.NewConnection()

	controller.views.Render(w, r, http.StatusOK, "connection", controller.connectionViews.Form(connection))
}

// loadConnection reads the connection of the {connectionID} path value; on
// failure it has written the response (404 or 500).
func (controller *Controllers) loadConnection(w http.ResponseWriter, r *http.Request) (store.Connection, bool) {
	id, err := strconv.ParseInt(r.PathValue("connectionID"), 10, 64)

	if err != nil {
		http.NotFound(w, r)

		return store.Connection{}, false
	}

	connection, err := controller.connections.Get(r.Context(), id)

	switch {
	case errors.Is(err, store.ErrNotFound):
		http.NotFound(w, r)

		return connection, false
	case err != nil:
		controller.serverError(w, r, err)

		return connection, false
	}

	return connection, true
}

func (controller *Controllers) ConnectionPage(w http.ResponseWriter, r *http.Request) {
	connection, ok := controller.loadConnection(w, r)

	if !ok {
		return
	}

	view := controller.connectionViews.Form(connection)
	view.Notice = notices[r.URL.Query().Get("notice")]

	if connection.Type == config.ChannelEmail {
		if destination, err := controller.verification.EmailDestination(r.Context(), connection.DestinationID); err == nil {
			view.Email.VerifiedAt = destination.VerifiedAt
		}
	}

	controller.views.Render(w, r, http.StatusOK, "connection", view)
}

func (controller *Controllers) CreateConnection(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	connection := store.Connection{Type: r.PostFormValue("type")}
	problems := requests.ReadConnectionForm(r, &connection, controller.registry)

	if available, reason := controller.notifications.ChannelAvailable(connection.Type); !available {
		problems["type"] = "This type is not available: " + reason + "."
	}

	var chatSecret, key string

	switch connection.Type {
	case config.ChannelEmail:
		// The address must have been confirmed in the email step of this form.
		destinationID, _ := strconv.ParseInt(r.PostFormValue("destination_id"), 10, 64)
		destination, err := controller.verification.EmailDestination(r.Context(), destinationID)

		if err != nil || !destination.Verified {
			problems["address"] = "Confirm the email address first."
		} else {
			connection.DestinationID, connection.Address, connection.Verified = destination.ID, destination.Address, true
			key = destination.Address
		}
	case config.ChannelTelegram, config.ChannelSlack:
		chat := requests.ReadChatSecret(r, controller.factory, connection.Type, "", len(problems) == 0)
		maps.Copy(problems, chat.Problems)
		chatSecret, key = chat.Secret, chat.Key
	}

	existing, err := controller.connections.FindDuplicate(r.Context(), connection.Type, key, 0)

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	if existing != nil {
		problems["duplicate"] = "already added"
	}

	if len(problems) > 0 {
		controller.renderConnectionForm(w, r, connection, problems, "", existing)

		return
	}

	if connection.Name == "" {
		connection.Name = controller.connections.DefaultName(r.Context(), connection, chatSecret)
	}

	if err := controller.connections.Create(r.Context(), connection, chatSecret); err != nil {
		controller.serverError(w, r, err)

		return
	}

	redirectAfterPost(w, r, "/notifications?notice=created")
}

func (controller *Controllers) UpdateConnection(w http.ResponseWriter, r *http.Request) {
	connection, ok := controller.loadConnection(w, r)

	if !ok {
		return
	}

	_ = r.ParseForm()

	problems := requests.ReadConnectionForm(r, &connection, controller.registry)
	chat := requests.ReadChatSecret(r, controller.factory, connection.Type, connection.Secret, len(problems) == 0)
	maps.Copy(problems, chat.Problems)
	chatSecret, key := chat.Secret, chat.Key

	if connection.Active && !connection.Verified {
		problems["active"] = "Confirm the email address first."
	}

	// A replaced token must not point to a chat another connection uses.
	existing, err := controller.connections.FindDuplicate(r.Context(), connection.Type, key, connection.ID)

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	if existing != nil {
		problems["duplicate"] = "already added"
	}

	if len(problems) > 0 {
		controller.renderConnectionForm(w, r, connection, problems, "", existing)

		return
	}

	if connection.Name == "" {
		connection.Name = controller.connections.DefaultName(r.Context(), connection, cmp.Or(chatSecret, connection.Secret))
	}

	if err := controller.connections.Update(r.Context(), connection, chatSecret); err != nil {
		controller.serverError(w, r, err)

		return
	}

	redirectAfterPost(w, r, fmt.Sprintf("/notifications/%d?notice=saved", connection.ID))
}

func (controller *Controllers) renderConnectionForm(w http.ResponseWriter, r *http.Request, connection store.Connection, problems map[string]string, failure string, existing *contracts.Duplicate) {
	view := controller.connectionViews.FormAgain(connection, problems, failure, existing, r.PostFormValue("address"))
	status := http.StatusOK

	if len(problems) > 0 {
		status = http.StatusUnprocessableEntity
	}

	controller.views.Render(w, r, status, "connection", view)
}

func (controller *Controllers) TestConnection(w http.ResponseWriter, r *http.Request) {
	connection, ok := controller.loadConnection(w, r)

	if !ok {
		return
	}

	// An unconfirmed address must not receive mail beyond the code.
	if !connection.Verified {
		controller.renderConnectionForm(w, r, connection, nil, "This address is not confirmed: no test message is sent.", nil)

		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)

	defer cancel()

	if err := controller.factory.SendTest(ctx, connection, controller.notifications.PublicURL); err != nil {
		controller.renderConnectionForm(w, r, connection, nil, "The test message was not delivered: "+err.Error(), nil)

		return
	}

	redirectAfterPost(w, r, fmt.Sprintf("/notifications/%d?notice=test-sent", connection.ID))
}

func (controller *Controllers) SetConnectionActive(w http.ResponseWriter, r *http.Request) {
	connection, ok := controller.loadConnection(w, r)

	if !ok {
		return
	}

	active := r.PostFormValue("active")

	if active != "0" && active != "1" {
		http.Error(w, "Invalid active value", http.StatusBadRequest)

		return
	}

	activate := active == "1"

	if activate && !connection.Verified {
		connection.Active = true
		controller.renderConnectionForm(w, r, connection, map[string]string{"active": "Confirm the email address first."}, "", nil)

		return
	}

	if err := controller.connections.SetActive(r.Context(), connection, activate); err != nil {
		controller.serverError(w, r, err)

		return
	}

	notice := "deactivated"

	if activate {
		notice = "activated"
	}

	redirectAfterPost(w, r, "/notifications?notice="+notice)
}

func (controller *Controllers) DeleteConnection(w http.ResponseWriter, r *http.Request) {
	connection, ok := controller.loadConnection(w, r)

	if !ok {
		return
	}

	if err := controller.connections.Delete(r.Context(), connection.ID); err != nil {
		controller.serverError(w, r, err)

		return
	}

	redirectAfterPost(w, r, "/notifications?notice=deleted")
}
