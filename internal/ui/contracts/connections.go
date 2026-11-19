package contracts

import (
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/platform/text"
	"github.com/devforth/exception-monitoring/internal/services"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

type TypeChoice struct {
	Value, Label, Reason string
	Available, Selected  bool
}

type Choice struct {
	Value   string
	Label   string
	Checked bool
}

// ConnectionView is what templates/notifications/connection.html shows: the form of a new
// or an existing connection, with errors after a failed save.
type ConnectionView struct {
	ID string
	// Waiting is how many messages wait for a retry after a failed attempt
	// (the "Retry now" button is shown for them).
	Waiting      int
	New          bool
	Name         string
	Type         string
	TypeLabel    string
	Email        services.EmailStep
	ChatID       string // shown; the bot token never is
	SlackChannel string // the channel of a Slack bot
	// The stored secrets, masked (helpers.MaskSecret) so the form can say
	// what is saved without showing it; empty when none is stored. An empty
	// field keeps the stored value.
	SavedWebhook       string
	SavedSlackToken    string
	SavedTelegramToken string
	AllProjects        bool
	Active             bool
	HideIssueLink      bool
	Verified           bool
	Types              []TypeChoice
	Projects           []Choice
	Levels             []Choice
	Errors             map[string]string
	Duplicate          *services.Duplicate // a chat already used by another connection
	Error              string              // a failure that is not about one field (sending, saving)
	Notice             string
}

// NamePlaceholder is what an empty name becomes for the shown type (app.js
// updates it as the type or the address changes).
func (view ConnectionView) NamePlaceholder() string {
	switch view.Type {
	case config.ChannelTelegram:
		return "the name of the chat, from Telegram"
	case config.ChannelSlack:
		return "Slack"
	}

	if local := text.EmailLocalPart(view.Email.Address); local != "" {
		return local
	}

	return "team"
}

// RestLocked tells the template to disable everything after the address
// until the address of an email connection is confirmed.
func (view ConnectionView) RestLocked() bool {
	return view.Type == config.ChannelEmail && view.Email.State != "verified"
}

type ConnectionRow struct {
	ID          string
	Name        string
	TypeLabel   string
	Destination string
	AllProjects bool
	Projects    []string // names, shown with their colors
	Levels      string
	Status      string
	StatusClass string // active, inactive or awaiting
	Active      bool
	Verified    bool
	Delivery    store.DeliveryHealth
}

type ConnectionsView struct {
	Rows   []ConnectionRow
	Notice string
}
