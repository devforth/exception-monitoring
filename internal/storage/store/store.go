// Package store is the API of the storage: the records, their rules and the
// Store interface, the one list of every operation (docs/storage-plan.md).
package store

import (
	"context"
	"time"
)

// Store reads and writes every record; nothing else touches a database.
type Store interface {
	// Agents: OAuth clients, codes, grants, personal tokens.

	RegisterOAuthClient(ctx context.Context, client OAuthClient) error
	GetOAuthClient(ctx context.Context, clientID string) (OAuthClient, error)
	CreateOAuthCode(ctx context.Context, codeHash string, code OAuthCode, now time.Time) error
	// TakeOAuthCode returns a code and deletes it: a code works once.
	TakeOAuthCode(ctx context.Context, codeHash string, now time.Time) (OAuthCode, error)
	CreateGrant(ctx context.Context, grant Grant, tokens GrantTokens) (int64, error)
	// ReplacePersonalToken keeps one personal token per user.
	ReplacePersonalToken(ctx context.Context, userID int64, accessHash string, now time.Time) error
	// RotateGrant swaps a grant's tokens; a spent refresh token revokes the grant.
	RotateGrant(ctx context.Context, clientID, refreshHash string, tokens GrantTokens, now time.Time) error
	// UseGrant finds the grant of a valid access token and records the use.
	UseGrant(ctx context.Context, accessHash string, now time.Time) (Grant, error)
	ListGrants(ctx context.Context, userID int64, now time.Time) ([]Grant, error)
	GetGrant(ctx context.Context, id int64) (Grant, error)
	DeleteGrant(ctx context.Context, id int64) error
	HasPersonalToken(ctx context.Context, userID int64) (bool, error)

	// Notification destinations and connections.

	CreateDestination(ctx context.Context, destination Destination, now time.Time) (int64, error)
	GetDestination(ctx context.Context, id int64) (Destination, error)
	// FindEmailDestination prefers a confirmed destination of the address.
	FindEmailDestination(ctx context.Context, address string) (Destination, error)
	UpdateDestinationSecret(ctx context.Context, id int64, secret string, now time.Time) error
	SetVerificationCode(ctx context.Context, destinationID int64, codeHash string, expiresAt, now time.Time) error
	// CheckVerificationCode counts an attempt; a match confirms the destination.
	CheckVerificationCode(ctx context.Context, destinationID int64, codeHash string, now time.Time) (VerifyResult, error)
	ListConnections(ctx context.Context) ([]Connection, error)
	GetConnection(ctx context.Context, id int64) (Connection, error)
	CreateConnection(ctx context.Context, connection Connection, now time.Time) (int64, error)
	UpdateConnection(ctx context.Context, connection Connection, now time.Time) error
	// DeleteConnection keeps the destination, so an address stays confirmed.
	DeleteConnection(ctx context.Context, id int64) error

	// Events and logs.

	// SaveEvent stores an event and creates or updates its issue.
	SaveEvent(ctx context.Context, event NewEvent) (SaveResult, error)
	SetIssueStatus(ctx context.Context, issueID int64, status string, actor Actor) error
	// SaveLogs stores logs; keepPerMessage > 0 keeps that many copies of a repeated message.
	SaveLogs(ctx context.Context, logs []NewLog, keepPerMessage int) error
	GetIssues(ctx context.Context, issueIDs []int64) ([]Issue, error)
	// SetIssuesStatus changes all or none (ErrNotFound for a missing ID).
	SetIssuesStatus(ctx context.Context, issueIDs []int64, status string, actor Actor) ([]Issue, error)
	IssueStatusHistory(ctx context.Context, issueID int64) ([]StatusChange, error)

	// Search index of logs (SQLite only).

	LogSearchReady(ctx context.Context) bool
	IndexLogsForSearch(ctx context.Context, batch int) (bool, error)
	LogsToIndex(ctx context.Context) (int, error)
	RefreshLogSearch(ctx context.Context) (bool, error)

	// Notification outbox.

	EnqueueOutbox(ctx context.Context, channels []string, kind string, payload, eventPayload []byte, now time.Time) error
	// DueOutbox returns a channel's due messages, oldest first.
	DueOutbox(ctx context.Context, channel string, now time.Time, limit int) ([]OutboxMessage, error)
	MarkOutboxSent(ctx context.Context, id int64, now time.Time) error
	MarkOutboxRetry(ctx context.Context, id int64, lastError string, next, now time.Time) error
	// RetryOutboxNow makes a channel's waiting messages due now.
	RetryOutboxNow(ctx context.Context, channel string, now time.Time) error
	MarkOutboxFailed(ctx context.Context, id int64, lastError string, now time.Time) error
	CleanOutbox(ctx context.Context, sentBefore, failedBefore time.Time) (int64, error)
	OutboxCounts(ctx context.Context) (map[string]int, error)
	// PendingChannels lists channels with messages to send, for a restart.
	PendingChannels(ctx context.Context) ([]string, error)
	ListOutbox(ctx context.Context, filter OutboxFilter) ([]OutboxRecord, error)
	CountOutbox(ctx context.Context, filter OutboxFilter) (int, error)
	DeliveryHealthByChannel(ctx context.Context) (map[string]DeliveryHealth, error)

	// Projects.

	ListProjects(ctx context.Context) ([]Project, error)
	GetProject(ctx context.Context, id int) (Project, error)
	CreateProject(ctx context.Context, project Project, now time.Time) (int, error)
	UpdateProject(ctx context.Context, project Project, now time.Time) error
	// DeleteProject deletes a project with all its data.
	DeleteProject(ctx context.Context, id int) error
	// SeedProject writes a project from PROJECTS: env sets its name and key.
	SeedProject(ctx context.Context, project Project, now time.Time) error

	// Reading issues, events and logs.

	CountIssues(ctx context.Context, filter IssueFilter) (int, error)
	ListIssues(ctx context.Context, filter IssueFilter) ([]Issue, error)
	GetIssue(ctx context.Context, issueID int64) (Issue, error)
	DeleteIssue(ctx context.Context, issueID int64) error
	UnresolvedCounts(ctx context.Context) (map[int]int, error)
	ListEvents(ctx context.Context, filter EventFilter) ([]EventSummary, error)
	// GetEvent finds an event by its SDK event_id within a project.
	GetEvent(ctx context.Context, projectID int, eventID string) (Event, error)
	LatestEvent(ctx context.Context, issueID int64) (Event, error)
	// CountLogs stops at LogCountLimit; more says there are more.
	CountLogs(ctx context.Context, filter LogFilter) (count int, more bool, err error)
	ListLogs(ctx context.Context, filter LogFilter) ([]LogRecord, error)
	// IssuesForLogs finds the issue of each log's request, by span.
	IssuesForLogs(ctx context.Context, logs []LogRecord) (map[int64]LogIssue, error)

	// The database.

	// DatabaseSize is what the database takes on disk.
	DatabaseSize(ctx context.Context) (int64, error)
	// CleanupHistory returns the latest runs of the cleanup by size, newest first.
	CleanupHistory(ctx context.Context, limit int) ([]CleanupRun, error)
	Ping(ctx context.Context) error
	Close() error

	// Sessions of the web UI (by the hash of the cookie).

	CreateSession(ctx context.Context, tokenHash string, userID int64, now, expiresAt time.Time) error
	SessionUser(ctx context.Context, tokenHash string, now time.Time) (User, error)
	DeleteSession(ctx context.Context, tokenHash string) error

	// Users.

	// CreateUser returns ErrUsernameTaken for a username taken in any case.
	CreateUser(ctx context.Context, user User, passwordHash string) (int64, error)
	GetUser(ctx context.Context, id int64) (User, error)
	FindUser(ctx context.Context, username string) (User, error)
	FindLogin(ctx context.Context, username string) (User, string, error)
	UserPassword(ctx context.Context, id int64) (string, error)
	Superadmin(ctx context.Context) (User, error)
	ListUsers(ctx context.Context) ([]User, error)
	UpdateUser(ctx context.Context, id int64, name, role string) error
	UpdateUserName(ctx context.Context, id int64, name string) error
	// SetPassword ends the user's other sessions.
	SetPassword(ctx context.Context, id int64, change PasswordChange) error
	DeleteUser(ctx context.Context, id int64) error
	RecordLogin(ctx context.Context, id int64, now time.Time) error
	// AdoptAgents gives agents from before users existed to userID.
	AdoptAgents(ctx context.Context, userID int64) error
}
