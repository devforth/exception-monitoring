package services

import "time"

// The results the services give and the inputs they take, shared by the UI
// (its templates show them) and the agents' tools.

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

// UserInput is what the user form sends.
type UserInput struct {
	Username string
	Name     string
	Role     string
}
