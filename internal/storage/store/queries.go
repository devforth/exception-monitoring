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
	ID           int64     `json:"-"`
	ProjectID    int       `json:"project_id"`
	IssueID      int64     `json:"issue_id"`
	EventID      string    `json:"event_id"`
	Timestamp    time.Time `json:"timestamp"`
	ReceivedAt   time.Time `json:"received_at"`
	Level        string    `json:"level"`
	Environment  string    `json:"environment"`
	Release      string    `json:"release"`
	TraceID      string    `json:"trace_id"`
	SpanID       string    `json:"span_id"`
	ParentSpanID string    `json:"parent_span_id"`
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
	SpanID         string          `json:"span_id"`
	Attributes     json.RawMessage `json:"attributes"`
}

// IssueFilter selects issues. Zero values mean "any".
type IssueFilter struct {
	ProjectIDs []int    // any of these projects (the UI filter takes several)
	Levels     []string // any of these levels
	Status     string
	Query      string // substring of the title or culprit
	Sort       string // a key of issueSorts; "" is last_seen
	// SeenSince and SeenUntil bound the date given by DateField to
	// [SeenSince, SeenUntil). Zero means no bound.
	SeenSince time.Time
	SeenUntil time.Time
	DateField string // DateAnyEvent (default), DateFirstSeen or DateLastSeen
	Limit     int
	Offset    int
}

// Which date of an issue a date range applies to.
const (
	// DateAnyEvent: the issue had at least one event in the range.
	DateAnyEvent = "event"
	// DateFirstSeen: the issue appeared in the range (new problems).
	DateFirstSeen = "first_seen"
	// DateLastSeen: the latest event of the issue is in the range.
	DateLastSeen = "last_seen"
)

// ValidDateField reports whether field is a supported IssueFilter.DateField value.
func ValidDateField(field string) bool {
	return field == "" || field == DateAnyEvent || field == DateFirstSeen || field == DateLastSeen
}

// dateCondition is the WHERE condition for the filter's date range, or "" without one.
func (filter IssueFilter) dateCondition() (string, []any) {
	if filter.SeenSince.IsZero() && filter.SeenUntil.IsZero() {
		return "", nil
	}

	// For "any event": first_seen/last_seen alone would miss an issue seen
	// before and after the range but not in it; events_by_issue_time makes the
	// subquery a range lookup.
	var column string

	switch filter.DateField {
	case DateFirstSeen:
		column = "first_seen"
	case DateLastSeen:
		column = "last_seen"
	default:
		column = "events.received_at"
	}

	var (
		conditions []string
		args       []any
	)

	if !filter.SeenSince.IsZero() {
		conditions, args = append(conditions, column+" >= ?"), append(args, formatTime(filter.SeenSince))
	}

	if !filter.SeenUntil.IsZero() {
		conditions, args = append(conditions, column+" < ?"), append(args, formatTime(filter.SeenUntil))
	}

	condition := strings.Join(conditions, " AND ")

	if column == "events.received_at" {
		condition = "EXISTS (SELECT 1 FROM events WHERE events.issue_id = issues.id AND " + condition + ")"
	}

	return condition, args
}

// projectName orders issues by the name of their project.
const projectName = "(SELECT name FROM projects WHERE projects.id = issues.project_id) COLLATE NOCASE"

// Issue sort orders: dates and counts newest or most first, "_asc" reverses
// them; the project A to Z, "project_desc" reverses it (the newest issues of
// a project first either way). The id tiebreaker keeps the order stable
// between pages.
var issueSorts = map[string]string{
	"id":              "id DESC",
	"id_asc":          "id ASC",
	"project":         projectName + " ASC, last_seen DESC, id DESC",
	"project_desc":    projectName + " DESC, last_seen DESC, id DESC",
	"last_seen":       "last_seen DESC, id DESC",
	"last_seen_asc":   "last_seen ASC, id ASC",
	"first_seen":      "first_seen DESC, id DESC",
	"first_seen_asc":  "first_seen ASC, id ASC",
	"event_count":     "event_count DESC, id DESC",
	"event_count_asc": "event_count ASC, id ASC",
}

// ValidIssueSort reports whether sort is a supported IssueFilter.Sort value.
func ValidIssueSort(sort string) bool {
	_, ok := issueSorts[sort]
	return sort == "" || ok
}

// where is the WHERE clause of the filter (with a leading space, or empty)
// and its arguments; ListIssues and CountIssues share it.
func (filter IssueFilter) where() (string, []any) {
	var (
		where []string
		args  []any
	)

	if condition, projectArgs := inCondition("project_id", filter.ProjectIDs); condition != "" {
		where, args = append(where, condition), append(args, projectArgs...)
	}

	if filter.Status != "" {
		where, args = append(where, "status = ?"), append(args, filter.Status)
	}

	if condition, levelArgs := inCondition("level", filter.Levels); condition != "" {
		where, args = append(where, condition), append(args, levelArgs...)
	}

	if filter.Query != "" {
		pattern := likePattern(filter.Query)

		where, args = append(where, `(title LIKE ? ESCAPE '\' OR culprit LIKE ? ESCAPE '\')`), append(args, pattern, pattern)
	}

	if condition, dateArgs := filter.dateCondition(); condition != "" {
		where, args = append(where, condition), append(args, dateArgs...)
	}

	if len(where) == 0 {
		return "", args
	}

	return " WHERE " + strings.Join(where, " AND "), args
}

// CountIssues counts the issues matching the filter (Limit and Offset are ignored).
func (store *Store) CountIssues(ctx context.Context, filter IssueFilter) (int, error) {
	where, args := filter.where()

	var count int

	err := store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM issues`+where, args...).Scan(&count)

	return count, err
}

// ListIssues returns issues matching the filter. Limit+1 rows are fetched by
// callers that need to know whether there is a next page.
func (store *Store) ListIssues(ctx context.Context, filter IssueFilter) ([]Issue, error) {
	where, args := filter.where()

	order, ok := issueSorts[filter.Sort]

	if !ok {
		order = issueSorts["last_seen"]
	}

	query := `SELECT ` + issueFields + ` FROM issues` + where

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
		`SELECT `+issueFields+` FROM issues WHERE id = ?`, issueID)
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

// GetEvent returns an event by its SDK event_id (32 lowercase hex, see
// sentry.NormalizeEventID), which is unique within a project.
func (store *Store) GetEvent(ctx context.Context, projectID int, eventID string) (Event, error) {
	return store.getEvent(ctx, `project_id = ? AND event_id = ?`, projectID, eventID)
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
	ProjectIDs []int    // any of these projects
	Levels     []string // any of these levels
	Query      string   // substring of the body
	TraceID    string
	SpanIDs    []string // any of these spans (used with TraceID for one request)
	Since      time.Time
	Until      time.Time
	BeforeID   int64
	Limit      int
	Offset     int // page offset for the UI; the API uses BeforeID
}

// where is the WHERE clause of the filter and its arguments, shared by
// ListLogs and CountLogs.
func (filter LogFilter) where() (string, []any) {
	var (
		where []string
		args  []any
	)

	if condition, projectArgs := inCondition("project_id", filter.ProjectIDs); condition != "" {
		where, args = append(where, condition), append(args, projectArgs...)
	}

	if condition, levelArgs := inCondition("level", filter.Levels); condition != "" {
		where, args = append(where, condition), append(args, levelArgs...)
	}

	if filter.Query != "" {
		where, args = append(where, `body LIKE ? ESCAPE '\'`), append(args, likePattern(filter.Query))
	}

	if filter.TraceID != "" {
		where, args = append(where, "trace_id = ?"), append(args, filter.TraceID)
	}

	if condition, spanArgs := inCondition("span_id", filter.SpanIDs); condition != "" {
		where, args = append(where, condition), append(args, spanArgs...)
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

	if len(where) == 0 {
		return "", args
	}

	return " WHERE " + strings.Join(where, " AND "), args
}

// CountLogs counts the logs matching the filter (Limit and Offset are ignored).
func (store *Store) CountLogs(ctx context.Context, filter LogFilter) (int, error) {
	where, args := filter.where()

	var count int

	err := store.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM logs`+where, args...).Scan(&count)

	return count, err
}

func (store *Store) ListLogs(ctx context.Context, filter LogFilter) ([]LogRecord, error) {
	where, args := filter.where()

	query := `SELECT id, project_id, timestamp, level, severity_number, body, trace_id, span_id, attributes FROM logs` + where +
		" ORDER BY id DESC LIMIT ? OFFSET ?"

	args = append(args, filter.Limit, filter.Offset)

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
			&record.Body, &record.TraceID, &record.SpanID, &attributes); err != nil {
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

	issue.ResolvedAt = parseOptionalTime(resolvedAt)

	return issue, nil
}

const eventSummaryColumns = `id, project_id, issue_id, event_id, timestamp, received_at, level, environment, release, trace_id, span_id, parent_span_id`

func scanEventSummary(row scanner, event *EventSummary, extra ...any) error {
	var timestamp, receivedAt string

	destinations := append([]any{&event.ID, &event.ProjectID, &event.IssueID, &event.EventID, &timestamp, &receivedAt,
		&event.Level, &event.Environment, &event.Release, &event.TraceID, &event.SpanID, &event.ParentSpanID}, extra...)

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

// parseOptionalTime reads a nullable time column: nil for NULL.
func parseOptionalTime(value sql.NullString) *time.Time {
	if !value.Valid {
		return nil
	}

	parsed := parseTime(value.String)

	return &parsed
}

// isUniqueViolation: the statement broke a UNIQUE constraint.
func isUniqueViolation(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// affectedOne turns "no row changed" of a statement into ErrNotFound.
func affectedOne(result sql.Result, err error) error {
	if err != nil {
		return err
	}

	if rows, err := result.RowsAffected(); err != nil || rows == 0 {
		return errors.Join(err, ErrNotFound)
	}

	return nil
}

// likePattern turns user text into a LIKE "contains" pattern, escaping % and _.
func likePattern(text string) string {
	replacer := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`)
	return "%" + replacer.Replace(text) + "%"
}

// inCondition is "column IN (...)" for a list of values, or "" for an empty
// list (no condition). column is a constant from the caller, never input.
func inCondition[T any](column string, values []T) (string, []any) {
	if len(values) == 0 {
		return "", nil
	}

	args := make([]any, 0, len(values))

	for _, value := range values {
		args = append(args, value)
	}

	return column + " IN (" + placeholders(len(args)) + ")", args
}

// placeholders returns "?, ?, ?" for an IN (...) list of count values.
func placeholders(count int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", count), ", ")
}

// LogIssue is the issue of the error raised in the same request as a log line.
type LogIssue struct {
	IssueID int64
	Title   string
}

// IssuesForLogs finds, for each log, the issue of an error from the same
// request: an event in the same trace whose span or parent span is the log's
// span. Logs without a match are absent from the result.
func (store *Store) IssuesForLogs(ctx context.Context, logs []LogRecord) (map[int64]LogIssue, error) {
	result := map[int64]LogIssue{}
	spans := map[string]bool{}

	for _, record := range logs {
		if record.SpanID != "" {
			spans[record.SpanID] = true
		}
	}

	if len(spans) == 0 {
		return result, nil
	}

	args := make([]any, 0, 2*len(spans))

	for span := range spans {
		args = append(args, span)
	}

	args = append(args, args...)
	list := placeholders(len(spans))

	// Newest events first, so a request with several errors links to the latest one.
	rows, err := store.DB.QueryContext(ctx, `
		SELECT events.trace_id, events.span_id, events.parent_span_id, issues.id, issues.title
		FROM events JOIN issues ON issues.id = events.issue_id
		WHERE events.span_id IN (`+list+`) OR events.parent_span_id IN (`+list+`)
		ORDER BY events.id DESC`, args...)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	bySpan := map[string]LogIssue{} // key: trace_id + "/" + span_id

	for rows.Next() {
		var traceID, spanID, parentSpanID string

		var issue LogIssue

		if err := rows.Scan(&traceID, &spanID, &parentSpanID, &issue.IssueID, &issue.Title); err != nil {
			return nil, err
		}

		for _, span := range []string{spanID, parentSpanID} {
			key := traceID + "/" + span

			if _, seen := bySpan[key]; !seen && span != "" {
				bySpan[key] = issue
			}
		}
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	for _, record := range logs {
		if issue, ok := bySpan[record.TraceID+"/"+record.SpanID]; ok && record.SpanID != "" {
			result[record.ID] = issue
		}
	}

	return result, nil
}

// Page trims rows fetched with one extra (Limit = limit+1) back to limit and
// reports whether there are more: the extra row tells that a next page exists.
func Page[T any](rows []T, limit int) (page []T, more bool) {
	if len(rows) > limit {
		return rows[:limit], true
	}

	return rows, false
}
