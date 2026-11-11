package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"
)

// ErrNotFound is returned when the requested row does not exist.
var ErrNotFound = errors.New("not found")

type Issue struct {
	ID         int64      `json:"id"`
	ProjectID  int        `json:"project_id"`
	Title      string     `json:"title"`
	Culprit    string     `json:"culprit"`
	Level      string     `json:"level"`
	Status     string     `json:"status"`
	EventCount int        `json:"event_count"`
	FirstSeen  time.Time  `json:"first_seen"`
	LastSeen   time.Time  `json:"last_seen"`
	ResolvedAt *time.Time `json:"resolved_at"`
}

// EventSummary is an event without its payload, for lists.
type EventSummary struct {
	ID          int64     `json:"-"`
	ProjectID   int       `json:"project_id"`
	IssueID     int64     `json:"issue_id"`
	EventID     string    `json:"event_id"`
	Timestamp   time.Time `json:"timestamp"`
	ReceivedAt  time.Time `json:"received_at"`
	Level       string    `json:"level"`
	Environment string    `json:"environment"`
	Release     string    `json:"release"`
	TraceID     string    `json:"trace_id"`
}

// Event is a stored event with the full JSON the SDK sent.
type Event struct {
	EventSummary
	Payload json.RawMessage `json:"payload"`
}

type LogRecord struct {
	ID             int64           `json:"id"`
	ProjectID      int             `json:"project_id"`
	Timestamp      time.Time       `json:"timestamp"`
	Level          string          `json:"level"`
	SeverityNumber int             `json:"severity_number"`
	Body           string          `json:"body"`
	TraceID        string          `json:"trace_id"`
	Attributes     json.RawMessage `json:"attributes"`
}

// IssueFilter selects issues. Zero values mean "any".
type IssueFilter struct {
	ProjectID int
	Status    string
	Level     string
	Query     string // substring of the title or culprit
	Sort      string // "last_seen" (default), "first_seen" or "event_count"
	Limit     int
	Offset    int
}

// Issue sort orders. The id tiebreaker keeps the order stable between pages.
var issueSorts = map[string]string{
	"last_seen":   "last_seen DESC, id DESC",
	"first_seen":  "first_seen DESC, id DESC",
	"event_count": "event_count DESC, id DESC",
}

// ValidIssueSort reports whether sort is a supported IssueFilter.Sort value.
func ValidIssueSort(sort string) bool {
	_, ok := issueSorts[sort]
	return sort == "" || ok
}

// ListIssues returns issues matching the filter. Limit+1 rows are fetched by
// callers that need to know whether there is a next page.
func (store *Store) ListIssues(ctx context.Context, filter IssueFilter) ([]Issue, error) {
	var (
		where []string
		args  []any
	)

	if filter.ProjectID != 0 {
		where, args = append(where, "project_id = ?"), append(args, filter.ProjectID)
	}

	if filter.Status != "" {
		where, args = append(where, "status = ?"), append(args, filter.Status)
	}

	if filter.Level != "" {
		where, args = append(where, "level = ?"), append(args, filter.Level)
	}

	if filter.Query != "" {
		pattern := likePattern(filter.Query)

		where, args = append(where, `(title LIKE ? ESCAPE '\' OR culprit LIKE ? ESCAPE '\')`), append(args, pattern, pattern)
	}

	order, ok := issueSorts[filter.Sort]

	if !ok {
		order = issueSorts["last_seen"]
	}

	query := `SELECT id, project_id, title, culprit, level, status, event_count, first_seen, last_seen, resolved_at FROM issues`

	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}

	query += " ORDER BY " + order + " LIMIT ? OFFSET ?"

	args = append(args, filter.Limit, filter.Offset)

	rows, err := store.DB.QueryContext(ctx, query, args...)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	issues := []Issue{}

	for rows.Next() {
		issue, err := scanIssue(rows)

		if err != nil {
			return nil, err
		}

		issues = append(issues, issue)
	}

	return issues, rows.Err()
}

func (store *Store) GetIssue(ctx context.Context, issueID int64) (Issue, error) {
	row := store.DB.QueryRowContext(ctx,
		`SELECT id, project_id, title, culprit, level, status, event_count, first_seen, last_seen, resolved_at FROM issues WHERE id = ?`, issueID)
	issue, err := scanIssue(row)

	if errors.Is(err, sql.ErrNoRows) {
		return issue, ErrNotFound
	}

	return issue, err
}

// DeleteIssue removes the issue and its events. If the error happens again,
// it starts a new issue.
func (store *Store) DeleteIssue(ctx context.Context, issueID int64) error {
	result, err := store.DB.ExecContext(ctx, `DELETE FROM issues WHERE id = ?`, issueID)

	if err != nil {
		return err
	}

	if count, err := result.RowsAffected(); err == nil && count == 0 {
		return ErrNotFound
	}

	return nil
}

// UnresolvedCounts returns the number of unresolved issues per project.
func (store *Store) UnresolvedCounts(ctx context.Context) (map[int]int, error) {
	rows, err := store.DB.QueryContext(ctx, `SELECT project_id, COUNT(*) FROM issues WHERE status = 'unresolved' GROUP BY project_id`)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	counts := map[int]int{}

	for rows.Next() {
		var projectID, count int

		if err := rows.Scan(&projectID, &count); err != nil {
			return nil, err
		}

		counts[projectID] = count
	}

	return counts, rows.Err()
}

// EventFilter selects events. Events are listed newest first; BeforeID
// continues a previous page (keyset pagination, stable while events arrive).
type EventFilter struct {
	IssueID  int64
	TraceID  string
	BeforeID int64
	Limit    int
}

func (store *Store) ListEvents(ctx context.Context, filter EventFilter) ([]EventSummary, error) {
	var (
		where []string
		args  []any
	)

	if filter.IssueID != 0 {
		where, args = append(where, "issue_id = ?"), append(args, filter.IssueID)
	}

	if filter.TraceID != "" {
		where, args = append(where, "trace_id = ?"), append(args, filter.TraceID)
	}

	if filter.BeforeID != 0 {
		where, args = append(where, "id < ?"), append(args, filter.BeforeID)
	}

	query := `SELECT ` + eventSummaryColumns + ` FROM events`

	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}

	query += " ORDER BY id DESC LIMIT ?"

	args = append(args, filter.Limit)

	rows, err := store.DB.QueryContext(ctx, query, args...)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	events := []EventSummary{}

	for rows.Next() {
		var event EventSummary

		if err := scanEventSummary(rows, &event); err != nil {
			return nil, err
		}

		events = append(events, event)
	}

	return events, rows.Err()
}

// GetEvent returns an event by its SDK event_id, which is unique within a project.
func (store *Store) GetEvent(ctx context.Context, projectID int, eventID string) (Event, error) {
	return store.getEvent(ctx, `project_id = ? AND event_id = ?`, projectID, strings.ToLower(strings.ReplaceAll(eventID, "-", "")))
}

// LatestEvent returns the most recent event of an issue.
func (store *Store) LatestEvent(ctx context.Context, issueID int64) (Event, error) {
	return store.getEvent(ctx, `id = (SELECT MAX(id) FROM events WHERE issue_id = ?)`, issueID)
}

func (store *Store) getEvent(ctx context.Context, where string, args ...any) (Event, error) {
	var (
		event   Event
		payload string
	)

	row := store.DB.QueryRowContext(ctx, `SELECT `+eventSummaryColumns+`, payload FROM events WHERE `+where, args...)
	err := scanEventSummary(row, &event.EventSummary, &payload)

	if errors.Is(err, sql.ErrNoRows) {
		return event, ErrNotFound
	}

	event.Payload = json.RawMessage(payload)

	return event, err
}

// LogFilter selects log records, newest first, with keyset pagination like events.
type LogFilter struct {
	ProjectID int
	Level     string
	Query     string // substring of the body
	TraceID   string
	Since     time.Time
	Until     time.Time
	BeforeID  int64
	Limit     int
}

func (store *Store) ListLogs(ctx context.Context, filter LogFilter) ([]LogRecord, error) {
	var (
		where []string
		args  []any
	)

	if filter.ProjectID != 0 {
		where, args = append(where, "project_id = ?"), append(args, filter.ProjectID)
	}

	if filter.Level != "" {
		where, args = append(where, "level = ?"), append(args, filter.Level)
	}

	if filter.Query != "" {
		where, args = append(where, `body LIKE ? ESCAPE '\'`), append(args, likePattern(filter.Query))
	}

	if filter.TraceID != "" {
		where, args = append(where, "trace_id = ?"), append(args, filter.TraceID)
	}

	if !filter.Since.IsZero() {
		where, args = append(where, "timestamp >= ?"), append(args, formatTime(filter.Since))
	}

	if !filter.Until.IsZero() {
		where, args = append(where, "timestamp < ?"), append(args, formatTime(filter.Until))
	}

	if filter.BeforeID != 0 {
		where, args = append(where, "id < ?"), append(args, filter.BeforeID)
	}

	query := `SELECT id, project_id, timestamp, level, severity_number, body, trace_id, attributes FROM logs`

	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}

	query += " ORDER BY id DESC LIMIT ?"

	args = append(args, filter.Limit)

	rows, err := store.DB.QueryContext(ctx, query, args...)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	logs := []LogRecord{}

	for rows.Next() {
		var (
			record                LogRecord
			timestamp, attributes string
		)

		if err := rows.Scan(&record.ID, &record.ProjectID, &timestamp, &record.Level, &record.SeverityNumber,
			&record.Body, &record.TraceID, &attributes); err != nil {
			return nil, err
		}

		record.Timestamp = parseTime(timestamp)
		record.Attributes = json.RawMessage(attributes)
		logs = append(logs, record)
	}

	return logs, rows.Err()
}

// scanner is what *sql.Row and *sql.Rows have in common.
type scanner interface {
	Scan(dest ...any) error
}

func scanIssue(row scanner) (Issue, error) {
	var (
		issue               Issue
		firstSeen, lastSeen string
		resolvedAt          sql.NullString
	)

	err := row.Scan(&issue.ID, &issue.ProjectID, &issue.Title, &issue.Culprit, &issue.Level, &issue.Status,
		&issue.EventCount, &firstSeen, &lastSeen, &resolvedAt)

	if err != nil {
		return issue, err
	}

	issue.FirstSeen = parseTime(firstSeen)
	issue.LastSeen = parseTime(lastSeen)

	if resolvedAt.Valid {
		resolved := parseTime(resolvedAt.String)
		issue.ResolvedAt = &resolved
	}

	return issue, nil
}

const eventSummaryColumns = `id, project_id, issue_id, event_id, timestamp, received_at, level, environment, release, trace_id`

func scanEventSummary(row scanner, event *EventSummary, extra ...any) error {
	var timestamp, receivedAt string

	destinations := append([]any{&event.ID, &event.ProjectID, &event.IssueID, &event.EventID, &timestamp, &receivedAt,
		&event.Level, &event.Environment, &event.Release, &event.TraceID}, extra...)

	if err := row.Scan(destinations...); err != nil {
		return err
	}

	event.Timestamp = parseTime(timestamp)
	event.ReceivedAt = parseTime(receivedAt)

	return nil
}

func parseTime(value string) time.Time {
	parsed, _ := time.Parse(timeFormat, value)
	return parsed
}

// likePattern turns user text into a LIKE "contains" pattern, escaping % and _.
func likePattern(text string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + replacer.Replace(text) + "%"
}
