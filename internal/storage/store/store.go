// Package store is the storage of the server as the rest of it sees it: the
// records (issues, events, logs, users, projects, notifications, agents),
// their rules (roles, statuses, filters) and the Store interface that reads
// and writes them. Implementations live in their own packages: sqlite today,
// kvstore (on goleveldb) as it replaces it (docs/storage-plan.md).
package store

import (
	"context"
	"time"
)

// Store reads and writes every record of the server. Services, the UI, the
// REST API, MCP, OAuth, notifications and retention use it, never an
// implementation directly.
type Store interface {
	// Agents: OAuth clients, codes and grants, personal tokens

	// RegisterOAuthClient stores a client, first dropping clients that never
	// connected within unusedClientAge.
	RegisterOAuthClient(ctx context.Context, client OAuthClient) error
	// GetOAuthClient reads a client; ErrNotFound when it is not registered.
	GetOAuthClient(ctx context.Context, clientID string) (OAuthClient, error)
	// CreateOAuthCode stores a code by its hash, dropping expired ones.
	CreateOAuthCode(ctx context.Context, codeHash string, code OAuthCode, now time.Time) error
	// TakeOAuthCode deletes a code and returns it: a code works once.
	// ErrNotFound when it does not exist or has expired.
	TakeOAuthCode(ctx context.Context, codeHash string, now time.Time) (OAuthCode, error)
	// CreateGrant stores a grant with its tokens.
	CreateGrant(ctx context.Context, grant Grant, tokens GrantTokens) (int64, error)
	// ReplacePersonalToken deletes the user's personal token, if any, and
	// stores a new one: one per user.
	ReplacePersonalToken(ctx context.Context, userID int64, accessHash string, now time.Time) error
	// RotateGrant swaps the tokens of the grant whose refresh token is
	// refreshHash, for clientID. ErrNotFound when no such grant exists or its
	// refresh token has expired. Reusing a spent token revokes the entire grant.
	RotateGrant(ctx context.Context, clientID, refreshHash string, tokens GrantTokens, now time.Time) error
	// UseGrant finds the grant of a valid access token and records the use.
	// ErrNotFound when the token is unknown or expired.
	UseGrant(ctx context.Context, accessHash string, now time.Time) (Grant, error)
	// ListGrants lists the connected agents and the personal token of a user,
	// newest first, dropping agents whose refresh token has expired (they would
	// have to connect again anyway).
	ListGrants(ctx context.Context, userID int64, now time.Time) ([]Grant, error)
	// GetGrant reads a grant (to check whose it is).
	GetGrant(ctx context.Context, id int64) (Grant, error)
	// DeleteGrant disconnects an agent: its tokens stop working at once.
	DeleteGrant(ctx context.Context, id int64) error
	// HasPersonalToken reports whether the user has a personal token.
	HasPersonalToken(ctx context.Context, userID int64) (bool, error)

	// Notification connections and their destinations

	CreateDestination(ctx context.Context, destination Destination, now time.Time) (int64, error)
	GetDestination(ctx context.Context, id int64) (Destination, error)
	// FindEmailDestination returns the destination of an address (case-insensitive),
	// a confirmed one if there is any.
	FindEmailDestination(ctx context.Context, address string) (Destination, error)
	// UpdateDestinationSecret replaces the encrypted tokens of a chat.
	UpdateDestinationSecret(ctx context.Context, id int64, secret string, now time.Time) error
	// SetVerificationCode stores the hash of a newly sent code, resets the
	// attempts and remembers when it was sent.
	SetVerificationCode(ctx context.Context, destinationID int64, codeHash string, expiresAt, now time.Time) error
	// CheckVerificationCode compares the hash of an entered code. A match marks
	// the destination confirmed (with the date) and clears the code; every check
	// counts as an attempt.
	CheckVerificationCode(ctx context.Context, destinationID int64, codeHash string, now time.Time) (VerifyResult, error)
	// ListConnections returns every connection with its projects, oldest first.
	ListConnections(ctx context.Context) ([]Connection, error)
	GetConnection(ctx context.Context, id int64) (Connection, error)
	// CreateConnection stores a new connection to a confirmed destination.
	CreateConnection(ctx context.Context, connection Connection, now time.Time) (int64, error)
	// UpdateConnection saves the name, projects, levels and active. An active
	// connection needs a confirmed destination.
	UpdateConnection(ctx context.Context, connection Connection, now time.Time) error
	// DeleteConnection removes the connection and its queued messages. The
	// destination stays, keeping an email address confirmed.
	DeleteConnection(ctx context.Context, id int64) error

	// Storing events and logs

	// SaveEvent stores the event and creates or updates its issue in one transaction.
	//
	// first_seen/last_seen use the server's receive time, not the SDK timestamp:
	// browser clocks are often wrong, and the issue list must be ordered by
	// when the server actually saw the problem.
	SaveEvent(ctx context.Context, event NewEvent) (SaveResult, error)
	// SetIssueStatus changes an issue's status (resolve, ignore, reopen).
	SetIssueStatus(ctx context.Context, issueID int64, status string, actor Actor) error
	// SaveLogs stores a batch of log records in one transaction. With
	// keepPerMessage > 0, each message of the batch then keeps only its newest
	// keepPerMessage logs, so a line logged in a loop replaces its old copies
	// instead of filling the database.
	SaveLogs(ctx context.Context, logs []NewLog, keepPerMessage int) error

	// Several issues at once

	// GetIssues returns summaries in one query, without loading event payloads.
	// Missing IDs are omitted; callers can report them alongside the found issues.
	GetIssues(ctx context.Context, issueIDs []int64) ([]Issue, error)
	// SetIssuesStatus applies one status to a batch atomically, recording actor
	// in the history of every issue whose status changes. Missing IDs
	// or invalid input leave every issue unchanged (ErrNotFound when one is
	// missing). Returned rows need no re-fetch. resolved_at keeps the first
	// resolve time while an issue stays resolved; any other status clears it.
	SetIssuesStatus(ctx context.Context, issueIDs []int64, status string, actor Actor) ([]Issue, error)

	// Status changes of issues

	// IssueStatusHistory lists the status changes of an issue, newest first.
	IssueStatusHistory(ctx context.Context, issueID int64) ([]StatusChange, error)

	// Search index of logs

	// LogSearchReady reports whether every log is in the search index.
	LogSearchReady(ctx context.Context) bool
	// IndexLogsForSearch adds up to batch of the logs from before the search
	// index to it, oldest first, and reports whether all of them are in. Each
	// batch is one short transaction, so ingest waits only for that long.
	IndexLogsForSearch(ctx context.Context, batch int) (bool, error)
	// LogsToIndex is how many logs are not in the search index yet.
	LogsToIndex(ctx context.Context) (int, error)
	// RefreshLogSearch starts filling the search index anew when it holds more
	// deleted logs than live ones (stale, see migration 22): it empties the index
	// in one short step and the background indexing (IndexLogsForSearch) adds the
	// logs again; until it is done, searches read logs. Reports whether it did.
	RefreshLogSearch(ctx context.Context) (bool, error)

	// Notification outbox

	// EnqueueOutbox adds the message once per channel, due immediately.
	EnqueueOutbox(ctx context.Context, channels []string, kind string, payload, eventPayload []byte, now time.Time) error
	// DueOutbox returns pending messages of a channel whose next attempt is due,
	// oldest first, so a channel sends in the order notifications happened.
	DueOutbox(ctx context.Context, channel string, now time.Time, limit int) ([]OutboxMessage, error)
	MarkOutboxSent(ctx context.Context, id int64, now time.Time) error
	// MarkOutboxRetry records a failed attempt and when to try again.
	MarkOutboxRetry(ctx context.Context, id int64, lastError string, next, now time.Time) error
	// RetryOutboxNow makes the waiting messages of a channel due now: after its
	// connection is fixed (a token, a scope), they need not wait for the next
	// step of the retry schedule. The channel's worker picks them up on its next
	// poll.
	RetryOutboxNow(ctx context.Context, channel string, now time.Time) error
	// MarkOutboxFailed gives up on a message after its last attempt.
	MarkOutboxFailed(ctx context.Context, id int64, lastError string, now time.Time) error
	// CleanOutbox deletes sent messages finished before sentBefore and failed ones
	// before failedBefore, so the table does not grow forever.
	CleanOutbox(ctx context.Context, sentBefore, failedBefore time.Time) (int64, error)
	// OutboxCounts returns the number of messages per status, for tests and diagnostics.
	OutboxCounts(ctx context.Context) (map[string]int, error)
	// PendingChannels lists channels with messages still to send, so their
	// workers start after a restart.
	PendingChannels(ctx context.Context) ([]string, error)
	// ListOutbox returns outbox rows for the delivery log, newest first.
	ListOutbox(ctx context.Context, filter OutboxFilter) ([]OutboxRecord, error)
	// CountOutbox counts the rows ListOutbox would page through.
	CountOutbox(ctx context.Context, filter OutboxFilter) (int, error)
	// DeliveryHealthByChannel returns the delivery health of every channel with
	// rows in the outbox. A sent row has no error (MarkOutboxSent clears it), so
	// the latest error belongs to a retrying or failed message.
	DeliveryHealthByChannel(ctx context.Context) (map[string]DeliveryHealth, error)

	// Projects

	// ListProjects returns every project by id.
	ListProjects(ctx context.Context) ([]Project, error)
	GetProject(ctx context.Context, id int) (Project, error)
	// CreateProject adds a project with the next free id (projects from
	// PROJECTS keep theirs through SeedProject).
	CreateProject(ctx context.Context, project Project, now time.Time) (int, error)
	// UpdateProject saves the name, key and retention of a project.
	UpdateProject(ctx context.Context, project Project, now time.Time) error
	// DeleteProject deletes a project with all its data: issues (and their
	// events), logs, and its place in notification connections.
	DeleteProject(ctx context.Context, id int) error
	// SeedProject writes a project from PROJECTS: created if missing, otherwise
	// its name and key are set to the env values (env is the source of truth for
	// them; retention set in the UI stays). A name or key taken by another
	// project is an error, so a typo in PROJECTS cannot take over another DSN.
	SeedProject(ctx context.Context, project Project, now time.Time) error

	// Reading issues, events and logs

	// CountIssues counts the issues matching the filter (Limit and Offset are ignored).
	CountIssues(ctx context.Context, filter IssueFilter) (int, error)
	// ListIssues returns issues matching the filter. Limit+1 rows are fetched by
	// callers that need to know whether there is a next page.
	ListIssues(ctx context.Context, filter IssueFilter) ([]Issue, error)
	GetIssue(ctx context.Context, issueID int64) (Issue, error)
	// DeleteIssue removes the issue and its events. If the error happens again,
	// it starts a new issue.
	DeleteIssue(ctx context.Context, issueID int64) error
	// UnresolvedCounts returns the number of unresolved issues per project.
	UnresolvedCounts(ctx context.Context) (map[int]int, error)
	ListEvents(ctx context.Context, filter EventFilter) ([]EventSummary, error)
	// GetEvent returns an event by its SDK event_id (32 lowercase hex, see
	// sentry.NormalizeEventID), which is unique within a project.
	GetEvent(ctx context.Context, projectID int, eventID string) (Event, error)
	// LatestEvent returns the most recent event of an issue.
	LatestEvent(ctx context.Context, issueID int64) (Event, error)
	// CountLogs counts the logs matching the filter (Limit and Offset are
	// ignored) up to domain.LogCountLimit; more says there are more than that.
	CountLogs(ctx context.Context, filter LogFilter) (count int, more bool, err error)
	ListLogs(ctx context.Context, filter LogFilter) ([]LogRecord, error)
	// IssuesForLogs finds, for each log, the issue of an error from the same
	// request: an event in the same trace whose span or parent span is the log's
	// span. Logs without a match are absent from the result.
	IssuesForLogs(ctx context.Context, logs []LogRecord) (map[int64]LogIssue, error)

	// Retention and the database size

	// DeleteEventsBefore deletes events received before cutoff, except the
	// latest event of each issue.
	DeleteEventsBefore(ctx context.Context, scope ProjectScope, cutoff time.Time) (int64, error)
	// TrimLogMessages keeps the newest keep logs of each repeated message. New
	// logs are trimmed as they arrive (SaveLogs); this applies a lowered limit to
	// what is already stored.
	TrimLogMessages(ctx context.Context, scope ProjectScope, keep int) (int64, error)
	// DeleteIssuesNotSeenSince deletes issues whose last event came before
	// cutoff, with all their events. The events go first, in batches: through
	// ON DELETE CASCADE they would all go in one statement, holding the write
	// lock for as long as that takes.
	DeleteIssuesNotSeenSince(ctx context.Context, scope ProjectScope, cutoff time.Time) (int64, error)
	// DeleteLogsBefore deletes logs with a timestamp before cutoff.
	DeleteLogsBefore(ctx context.Context, scope ProjectScope, cutoff time.Time) (int64, error)
	// TrimIssueEvents keeps the newest keep events of every issue. The issue's
	// event_count is left as is: it counts every event ever received, which also
	// picks the issues that may have more than keep events stored; each is then
	// trimmed through the events_by_issue_time index.
	TrimIssueEvents(ctx context.Context, scope ProjectScope, keep int) (int64, error)
	// DeleteOldest deletes about limit of the oldest records, events (but not the
	// latest of an issue) and logs together in time order. Used to bring the database under its size limit,
	// so the batch is small: it should not delete much more than needed.
	// Returns 0 when there is nothing left to delete.
	DeleteOldest(ctx context.Context, limit int) (int64, error)
	// CountRecords returns how many events and logs are stored.
	CountRecords(ctx context.Context) (int64, error)
	// DatabaseSize returns the bytes used by data (the file size minus free pages).
	DatabaseSize(ctx context.Context) (int64, error)
	// ReleaseFreeSpace returns the pages freed by deletes to the file system.
	ReleaseFreeSpace(ctx context.Context) error

	// Sessions of the web UI

	// CreateSession stores a login session. tokenHash is the hash of the cookie
	// value; the cookie value itself is never stored.
	CreateSession(ctx context.Context, tokenHash string, userID int64, now, expiresAt time.Time) error
	// SessionUser returns the user of a session that has not expired;
	// ErrNotFound otherwise.
	SessionUser(ctx context.Context, tokenHash string, now time.Time) (User, error)
	DeleteSession(ctx context.Context, tokenHash string) error

	// The database itself

	Ping(ctx context.Context) error
	Close() error

	// Users

	// CreateUser stores a new user with a password hash; ErrUsernameTaken when
	// the username (in any case) exists.
	CreateUser(ctx context.Context, user User, passwordHash string) (int64, error)
	GetUser(ctx context.Context, id int64) (User, error)
	// FindUser finds a user by username, in any case.
	FindUser(ctx context.Context, username string) (User, error)
	// FindLogin finds a user by username with their password hash, for a login.
	FindLogin(ctx context.Context, username string) (User, string, error)
	// UserPassword returns a user's password hash, to check a password.
	UserPassword(ctx context.Context, id int64) (string, error)
	// Superadmin returns the superadmin; ErrNotFound when there is none yet.
	Superadmin(ctx context.Context) (User, error)
	// ListUsers lists the users by role (superadmin first), then username.
	ListUsers(ctx context.Context) ([]User, error)
	// UpdateUser changes a user's name and role.
	UpdateUser(ctx context.Context, id int64, name, role string) error
	// UpdateUserName changes only the name, so a role changed meanwhile stays.
	UpdateUserName(ctx context.Context, id int64, name string) error
	// SetPassword replaces a user's password and ends their other sessions.
	SetPassword(ctx context.Context, id int64, change PasswordChange) error
	// DeleteUser removes a user with their sessions and agents (foreign keys);
	// the status changes they made keep their name.
	DeleteUser(ctx context.Context, id int64) error
	// RecordLogin notes the time of a user's login.
	RecordLogin(ctx context.Context, id int64, now time.Time) error
	// AdoptAgents gives agents connected before there were users to userID (the
	// superadmin), so they keep working.
	AdoptAgents(ctx context.Context, userID int64) error
}
