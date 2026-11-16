package controllers

import (
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/services"
	"github.com/devforth/exception-monitoring/internal/storage/store"
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
	"retried":     "The waiting messages are being sent again now.",
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
	id, ok := controller.pathID(w, r, "connectionID")

	if !ok {
		return store.Connection{}, false
	}

	connection, err := controller.connections.Get(r.Context(), id)

	return connection, !controller.failed(w, r, err)
}

func (controller *Controllers) ConnectionPage(w http.ResponseWriter, r *http.Request) {
	connection, ok := controller.loadConnection(w, r)

	if !ok {
		return
	}

	view := controller.connectionViews.Form(connection)
	view.Notice = notices[r.URL.Query().Get("notice")]

	health, err := controller.connections.DeliveryHealth(r.Context())

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	view.Waiting = health[strconv.FormatInt(connection.ID, 10)].Retrying

	if connection.Type == config.ChannelEmail {
		if destination, err := controller.verification.EmailDestination(r.Context(), connection.DestinationID); err == nil {
			view.Email.VerifiedAt = destination.VerifiedAt
		}
	}

	controller.views.Render(w, r, http.StatusOK, "connection", view)
}

func (controller *Controllers) CreateConnection(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()

	saved, err := controller.connections.Save(r.Context(), r.PostForm, store.Connection{})

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	if len(saved.Problems) > 0 {
		controller.renderConnectionForm(w, r, saved.Connection, saved.Problems, "", saved.Duplicate)

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

	saved, err := controller.connections.Save(r.Context(), r.PostForm, connection)

	if err != nil {
		controller.serverError(w, r, err)

		return
	}

	if len(saved.Problems) > 0 {
		controller.renderConnectionForm(w, r, saved.Connection, saved.Problems, "", saved.Duplicate)

		return
	}

	redirectAfterPost(w, r, fmt.Sprintf("/notifications/%d?notice=saved", connection.ID))
}

func (controller *Controllers) renderConnectionForm(w http.ResponseWriter, r *http.Request, connection store.Connection, problems map[string]string, failure string, existing *services.Duplicate) {
	view := controller.connectionViews.FormAgain(connection, problems, failure, existing, r.PostFormValue("address"))
	status := http.StatusOK

	if len(problems) > 0 {
		status = http.StatusUnprocessableEntity
	}

	controller.views.Render(w, r, status, "connection", view)
}

// RetryConnection sends the messages waiting for a retry again now, then
// goes back to the page the button was on.
func (controller *Controllers) RetryConnection(w http.ResponseWriter, r *http.Request) {
	connection, ok := controller.loadConnection(w, r)

	if !ok {
		return
	}

	if err := controller.connections.RetryNow(r.Context(), connection.ID); err != nil {
		controller.serverError(w, r, err)

		return
	}

	target := requests.SafeNext(r.PostFormValue("return"))

	if r.PostFormValue("return") == "" {
		target = fmt.Sprintf("/notifications/%d", connection.ID)
	}

	redirectAfterPost(w, r, target+"?notice=retried")
}

func (controller *Controllers) TestConnection(w http.ResponseWriter, r *http.Request) {
	connection, ok := controller.loadConnection(w, r)

	if !ok {
		return
	}

	// An unconfirmed address must not receive mail beyond the code.
	err := controller.connections.SendTest(r.Context(), connection)

	switch {
	case errors.Is(err, store.ErrNotVerified):
		controller.renderConnectionForm(w, r, connection, nil, "This address is not confirmed: no test message is sent.", nil)
	case err != nil:
		controller.renderConnectionForm(w, r, connection, nil, "The test message was not delivered: "+err.Error(), nil)
	default:
		redirectAfterPost(w, r, fmt.Sprintf("/notifications/%d?notice=test-sent", connection.ID))
	}
}

// TestChat sends a test message with the chat settings in the form, before
// they are saved, and answers with the chat-test fragment next to the button.
// On an existing connection, fields left empty keep the stored tokens.
func (controller *Controllers) TestChat(w http.ResponseWriter, r *http.Request) {
	var existing store.Connection

	_ = r.ParseForm()
	channelType := r.PostFormValue("type")

	if r.PathValue("connectionID") != "" {
		connection, ok := controller.loadConnection(w, r)

		if !ok {
			return
		}

		existing, channelType = connection, connection.Type
	}

	result := controller.connections.TestChat(r.Context(), r.PostForm, channelType, existing)

	controller.views.RenderPartial(w, r, "connection", "chat-test", result)
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

	if err := controller.connections.SetActive(r.Context(), connection, activate); errors.Is(err, store.ErrNotVerified) {
		connection.Active = true
		controller.renderConnectionForm(w, r, connection, map[string]string{"active": "Confirm the email address first."}, "", nil)

		return
	} else if err != nil {
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
