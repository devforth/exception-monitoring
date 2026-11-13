package contracts

import (
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/helpers"
)

// MaxConnectionName is the longest name a connection can have.
const MaxConnectionName = 100

type TypeChoice struct {
	Value, Label, Reason string
	Available, Selected  bool
}

type Choice struct {
	Value   string
	Label   string
	Checked bool
}

// EmailStep is the address part of the form, swapped by htmx as it moves on:
// State "input" (enter an address), "unconfirmed" (an existing connection
// whose address is not confirmed yet: send a code), "code" (enter the mailed
// code) or "verified" (confirmed: the rest of the form opens). Fixed is set
// for an existing connection, whose address cannot change.
type EmailStep struct {
	State         string
	Address       string
	DestinationID int64
	VerifiedAt    time.Time
	Fixed         bool
	Duplicate     *Duplicate // the connection that already uses the address
	Error         string
	Notice        string
}

// Duplicate names the connection that already sends to an address or chat,
// so the error can link to it.
type Duplicate struct {
	ID   int64
	Name string
}

// ChatTest is the result of a test message sent from the form.
type ChatTest struct {
	Sent    bool
	Message string
}

// ConnectionView is what templates/connection.html shows: the form of a new
// or an existing connection, with errors after a failed save.
type ConnectionView struct {
	ID int64
	// Waiting is how many messages wait for a retry after a failed attempt
	// (the "Retry now" button is shown for them).
	Waiting      int
	New          bool
	Name         string
	Type         string
	TypeLabel    string
	Email        EmailStep
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
	Duplicate          *Duplicate // a chat already used by another connection
	Error              string     // a failure that is not about one field (sending, saving)
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

	if local := helpers.EmailLocalPart(view.Email.Address); local != "" {
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
	ID          int64
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
	Rows      []ConnectionRow
	CanCreate bool
	Notice    string
}
