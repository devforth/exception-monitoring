package store

import (
	"errors"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/config"
)

// Destination is where messages go: an email address or a chat. An email
// address is confirmed with a code once; the confirmation stays with the
// address, so a new connection to it needs no new code.
type Destination struct {
	ID           int64
	Type         string // config.ChannelEmail, ChannelTelegram or ChannelSlack
	Address      string // email, lowercase
	Secret       string // encrypted tokens of a chat, see package secret
	Verified     bool
	VerifiedAt   time.Time
	VerifySentAt time.Time // when the last code was sent, for the resend limit
	CreatedAt    time.Time
}

// Connection is a notification rule managed in the UI: a destination with
// the projects and levels it receives. The destination fields (Type,
// Address, Secret, Verified) are filled when reading, for convenience; they
// are changed through the destination.
type Connection struct {
	ID            int64
	Name          string
	DestinationID int64
	AllProjects   bool  // every project, including ones added to PROJECTS later
	ProjectIDs    []int // used when AllProjects is false
	Levels        []string
	Active        bool
	HideIssueLink bool // messages carry no link to the panel (default for chats, which may be public)
	CreatedAt     time.Time

	Type     string
	Address  string
	Secret   string
	Verified bool
}

// NewConnection is what a new connection of channelType starts with: every
// project, the levels recorded by default (config.Recording.RecordedEventLevels),
// active. Issue links go to email by default; a chat may be public, so it
// gets them only when asked.
func NewConnection(channelType string, recording config.Recording) Connection {
	return Connection{Type: channelType, AllProjects: true, Levels: recording.RecordedEventLevels(), Active: true, HideIssueLink: channelType != config.ChannelEmail}
}

// ErrNotVerified is returned when a connection would send to an email
// address that was not confirmed.
var ErrNotVerified = errors.New("the email address is not confirmed")

// MaxVerifyAttempts is how many wrong codes are accepted before a new code
// must be sent, so a 6-digit code cannot be guessed.
const MaxVerifyAttempts = 5

// VerifyResult is the outcome of checking a verification code.
type VerifyResult int

const (
	VerifyOK VerifyResult = iota
	VerifyWrong
	VerifyExpired      // no code, or it is too old: send a new one
	VerifyTooManyTries // MaxVerifyAttempts wrong codes: send a new one
)
