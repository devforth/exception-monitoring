package presenters

import (
	"slices"
	"strconv"
	"strings"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/notify"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/helpers"
)

var channelLabels = map[string]string{
	config.ChannelEmail:    "Email",
	config.ChannelTelegram: "Telegram",
	config.ChannelSlack:    "Slack",
}

// Connections presents notification connections: the list and the form.
type Connections struct {
	// Factory decrypts chat secrets to show the chat id (never the token).
	Factory       notify.Factory
	Notifications config.Notifications
	Projects      config.ProjectList
	ProjectName   ProjectName
}

// List is the notifications page; health is the delivery health by connection id.
func (presenter Connections) List(connections []store.Connection, health map[string]store.DeliveryHealth, notice string) contracts.ConnectionsView {
	view := contracts.ConnectionsView{Notice: notice}

	for _, connection := range connections {
		row := presenter.Row(connection)
		row.Delivery = health[strconv.FormatInt(connection.ID, 10)]
		view.Rows = append(view.Rows, row)
	}

	for _, channelType := range config.AllChannelTypes {
		if available, _ := presenter.Notifications.ChannelAvailable(channelType); available {
			view.CanCreate = true
		}
	}

	return view
}

func (presenter Connections) Row(connection store.Connection) contracts.ConnectionRow {
	row := contracts.ConnectionRow{ID: connection.ID, Name: connection.Name, TypeLabel: channelLabels[connection.Type], Levels: strings.Join(connection.Levels, ", ")}
	row.Active, row.Verified = connection.Active, connection.Verified

	switch connection.Type {
	case config.ChannelEmail:
		row.Destination = connection.Address
	case config.ChannelTelegram:
		var tokens notify.TelegramSecret

		if presenter.Factory.DecryptSecret(connection.Secret, &tokens) == nil {
			row.Destination = "chat " + tokens.ChatID
		} else {
			row.Destination = "token cannot be decrypted (SECRET_KEY changed?)"
		}
	case config.ChannelSlack:
		var tokens notify.SlackSecret

		switch {
		case presenter.Factory.DecryptSecret(connection.Secret, &tokens) != nil:
			row.Destination = "token cannot be decrypted (SECRET_KEY changed?)"
		case tokens.BotToken != "":
			row.Destination = "bot in " + tokens.Channel + ", prompt in thread"
		default:
			row.Destination = "incoming webhook"
		}
	}

	row.AllProjects = connection.AllProjects

	for _, projectID := range connection.ProjectIDs {
		row.Projects = append(row.Projects, presenter.ProjectName(projectID))
	}

	switch {
	case !connection.Verified:
		row.Status, row.StatusClass = "awaiting confirmation", "awaiting"
	case connection.Active:
		row.Status, row.StatusClass = "active", "active"
	default:
		row.Status, row.StatusClass = "inactive", "inactive"
	}

	return row
}

// NewConnection is what the form of a new connection starts with.
func (presenter Connections) NewConnection() store.Connection {
	connection := store.Connection{AllProjects: true, Levels: []string{"error", "fatal"}, Active: true}

	// Preselect the first type that can be used.
	for _, channelType := range config.AllChannelTypes {
		if available, _ := presenter.Notifications.ChannelAvailable(channelType); available {
			connection.Type = channelType

			break
		}
	}

	// Issue links go to email by default; a chat may be public, so it gets
	// them only when asked (app.js follows the type chosen in the form).
	connection.HideIssueLink = connection.Type != config.ChannelEmail

	return connection
}

// Form fills the form from a connection (empty for a new one).
func (presenter Connections) Form(connection store.Connection) contracts.ConnectionView {
	view := contracts.ConnectionView{
		ID: connection.ID, New: connection.ID == 0, Name: connection.Name, Type: connection.Type,
		TypeLabel: channelLabels[connection.Type], AllProjects: connection.AllProjects,
		HideIssueLink: connection.HideIssueLink,
		Active:        connection.Active, Verified: connection.Verified, Errors: map[string]string{},
		Email: contracts.EmailStep{State: "input", Address: connection.Address, DestinationID: connection.DestinationID},
	}

	if connection.Type == config.ChannelEmail && !view.New {
		view.Email.Fixed = true
		view.Email.State = "unconfirmed"
	}

	if connection.Type == config.ChannelEmail && connection.Verified {
		view.Email.State = "verified"
	}

	for _, channelType := range config.AllChannelTypes {
		available, reason := presenter.Notifications.ChannelAvailable(channelType)
		view.Types = append(view.Types, contracts.TypeChoice{
			Value: channelType, Label: channelLabels[channelType], Reason: reason,
			Available: available, Selected: channelType == connection.Type,
		})
	}

	for _, project := range presenter.Projects.List() {
		view.Projects = append(view.Projects, contracts.Choice{
			Value: strconv.Itoa(project.ID), Label: project.Name, Checked: slices.Contains(connection.ProjectIDs, project.ID),
		})
	}

	for _, level := range config.Levels {
		view.Levels = append(view.Levels, contracts.Choice{Value: level, Label: level, Checked: slices.Contains(connection.Levels, level)})
	}

	if connection.Type == config.ChannelTelegram && connection.Secret != "" {
		var tokens notify.TelegramSecret

		if presenter.Factory.DecryptSecret(connection.Secret, &tokens) == nil {
			view.ChatID, view.SavedTelegramToken = tokens.ChatID, helpers.MaskSecret(tokens.BotToken)
		}
	}

	if connection.Type == config.ChannelSlack && connection.Secret != "" {
		var tokens notify.SlackSecret

		if presenter.Factory.DecryptSecret(connection.Secret, &tokens) == nil {
			view.SlackChannel = tokens.Channel
			view.SavedWebhook, view.SavedSlackToken = helpers.MaskSecret(tokens.WebhookURL), helpers.MaskSecret(tokens.BotToken)
		}
	}

	return view
}

// FormAgain is the form shown again after a failed save or test: problems
// are the field errors, failure an error that is not about one field,
// existing the connection that already sends there and typedAddress the
// address field as posted.
func (presenter Connections) FormAgain(connection store.Connection, problems map[string]string, failure string, existing *contracts.Duplicate, typedAddress string) contracts.ConnectionView {
	view := presenter.Form(connection)
	view.Error = failure
	view.Duplicate = existing

	if problems != nil {
		view.Errors = problems
	}

	// Keep the email step where it was: a confirmed address stays confirmed.
	if connection.ID == 0 && connection.Type == config.ChannelEmail {
		view.Email.Address = typedAddress

		if connection.DestinationID != 0 {
			view.Email.State = "verified"
		}

		// The address is taken: back to the address field, with a link.
		if existing != nil {
			view.Email = contracts.EmailStep{State: "input", Address: connection.Address, Duplicate: existing}
			view.Duplicate = nil
		}
	}

	return view
}
