package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Project is a row of the projects table. Nil retention fields use the
// global settings.
type Project struct {
	ID                      int
	Name                    string
	PublicKey               string
	RetentionEvents         *time.Duration
	RetentionLogs           *time.Duration
	RetentionIssues         *time.Duration
	RetentionEventsPerIssue *int
	RetentionLogsPerMessage *int
	// EventLevels and LogLevels: the levels recorded; nil uses the defaults.
	EventLevels *[]string
	LogLevels   *[]string
	Color       string // "#rrggbb", or "" for the palette color
	CreatedAt   time.Time
}

// ErrProjectConflict: the name or the key is already used by another project.
var ErrProjectConflict = errors.New("project name or key already used")

const projectColumns = `id, name, public_key, retention_events, retention_logs, retention_issues, retention_events_per_issue, retention_logs_per_message, event_levels, log_levels, color, created_at`

func scanProject(row interface{ Scan(...any) error }) (Project, error) {
	var (
		project                                    Project
		events, logs, issues, perIssue, perMessage sql.NullInt64
		eventLevels, logLevels                     sql.NullString
		createdAt                                  string
	)

	if err := row.Scan(&project.ID, &project.Name, &project.PublicKey, &events, &logs, &issues, &perIssue, &perMessage, &eventLevels, &logLevels, &project.Color, &createdAt); err != nil {
		return project, err
	}

	project.RetentionEvents = durationFromSeconds(events)
	project.RetentionLogs = durationFromSeconds(logs)
	project.RetentionIssues = durationFromSeconds(issues)

	project.RetentionEventsPerIssue = intFromNull(perIssue)
	project.RetentionLogsPerMessage = intFromNull(perMessage)
	project.EventLevels = levelsFromNull(eventLevels)
	project.LogLevels = levelsFromNull(logLevels)

	project.CreatedAt, _ = time.Parse(timeFormat, createdAt)

	return project, nil
}

func levelsFromNull(value sql.NullString) *[]string {
	if !value.Valid {
		return nil
	}

	levels := []string{}

	if value.String != "" {
		levels = strings.Split(value.String, ",")
	}

	return &levels
}

func levelsOrNull(levels *[]string) any {
	if levels == nil {
		return nil
	}

	return strings.Join(*levels, ",")
}

func intFromNull(value sql.NullInt64) *int {
	if !value.Valid {
		return nil
	}

	count := int(value.Int64)

	return &count
}

func durationFromSeconds(value sql.NullInt64) *time.Duration {
	if !value.Valid {
		return nil
	}

	duration := time.Duration(value.Int64) * time.Second

	return &duration
}

func secondsOf(duration *time.Duration) any {
	if duration == nil {
		return nil
	}

	return int64(*duration / time.Second)
}

func intOrNull(value *int) any {
	if value == nil {
		return nil
	}

	return *value
}

// ListProjects returns every project by id.
func (store *Store) ListProjects(ctx context.Context) ([]Project, error) {
	rows, err := store.DB.QueryContext(ctx, `SELECT `+projectColumns+` FROM projects ORDER BY id`)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var projects []Project

	for rows.Next() {
		project, err := scanProject(rows)

		if err != nil {
			return nil, err
		}

		projects = append(projects, project)
	}

	return projects, rows.Err()
}

func (store *Store) GetProject(ctx context.Context, id int) (Project, error) {
	project, err := scanProject(store.DB.QueryRowContext(ctx, `SELECT `+projectColumns+` FROM projects WHERE id = ?`, id))

	if errors.Is(err, sql.ErrNoRows) {
		return project, ErrNotFound
	}

	return project, err
}

// CreateProject adds a project with the next free id (projects from
// PROJECTS keep theirs through SeedProject).
func (store *Store) CreateProject(ctx context.Context, project Project, now time.Time) (int, error) {
	result, err := store.DB.ExecContext(ctx, `
		INSERT INTO projects (name, public_key, retention_events, retention_logs, retention_issues, retention_events_per_issue, retention_logs_per_message,
			event_levels, log_levels, color, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		project.Name, project.PublicKey, secondsOf(project.RetentionEvents), secondsOf(project.RetentionLogs),
		secondsOf(project.RetentionIssues), intOrNull(project.RetentionEventsPerIssue), intOrNull(project.RetentionLogsPerMessage),
		levelsOrNull(project.EventLevels), levelsOrNull(project.LogLevels), project.Color, formatTime(now), formatTime(now))

	if err != nil {
		return 0, conflictError(err)
	}

	newID, err := result.LastInsertId()

	return int(newID), err
}

// UpdateProject saves the name, key and retention of a project.
func (store *Store) UpdateProject(ctx context.Context, project Project, now time.Time) error {
	result, err := store.DB.ExecContext(ctx, `
		UPDATE projects SET name = ?, public_key = ?, retention_events = ?, retention_logs = ?, retention_issues = ?,
			retention_events_per_issue = ?, retention_logs_per_message = ?, event_levels = ?, log_levels = ?, color = ?, updated_at = ?
		WHERE id = ?`,
		project.Name, project.PublicKey, secondsOf(project.RetentionEvents), secondsOf(project.RetentionLogs),
		secondsOf(project.RetentionIssues), intOrNull(project.RetentionEventsPerIssue), intOrNull(project.RetentionLogsPerMessage),
		levelsOrNull(project.EventLevels), levelsOrNull(project.LogLevels), project.Color, formatTime(now), project.ID)

	if err != nil {
		return conflictError(err)
	}

	if updated, _ := result.RowsAffected(); updated == 0 {
		return ErrNotFound
	}

	return nil
}

// DeleteProject deletes a project with all its data: issues (and their
// events), logs, and its place in notification connections.
func (store *Store) DeleteProject(ctx context.Context, id int) error {
	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return err
	}

	defer func() { _ = transaction.Rollback() }() // no-op after Commit

	for _, query := range []string{
		`DELETE FROM logs WHERE project_id = ?`,
		`DELETE FROM events WHERE project_id = ?`,
		`DELETE FROM issues WHERE project_id = ?`,
		`DELETE FROM notification_connection_projects WHERE project_id = ?`,
	} {
		if _, err := transaction.ExecContext(ctx, query, id); err != nil {
			return err
		}
	}

	result, err := transaction.ExecContext(ctx, `DELETE FROM projects WHERE id = ?`, id)

	if err != nil {
		return err
	}

	if deleted, _ := result.RowsAffected(); deleted == 0 {
		return ErrNotFound
	}

	return transaction.Commit()
}

// SeedProject writes a project from PROJECTS: created if missing, otherwise
// its name and key are set to the env values (env is the source of truth for
// them; retention set in the UI stays). A name or key taken by another
// project is an error, so a typo in PROJECTS cannot take over another DSN.
func (store *Store) SeedProject(ctx context.Context, project Project, now time.Time) error {
	result, err := store.DB.ExecContext(ctx,
		`UPDATE projects SET name = ?, public_key = ?, updated_at = ? WHERE id = ? AND (name != ? OR public_key != ?)`,
		project.Name, project.PublicKey, formatTime(now), project.ID, project.Name, project.PublicKey)

	if err != nil {
		return fmt.Errorf("project %d %q: %w", project.ID, project.Name, conflictError(err))
	}

	if updated, _ := result.RowsAffected(); updated > 0 {
		return nil
	}

	_, err = store.DB.ExecContext(ctx, `
		INSERT INTO projects (id, name, public_key, created_at, updated_at) VALUES (?, ?, ?, ?, ?)
		ON CONFLICT (id) DO NOTHING`,
		project.ID, project.Name, project.PublicKey, formatTime(now), formatTime(now))

	if err != nil {
		return fmt.Errorf("project %d %q: %w", project.ID, project.Name, conflictError(err))
	}

	return nil
}

func conflictError(err error) error {
	if isUniqueViolation(err) {
		return fmt.Errorf("%w: %v", ErrProjectConflict, err)
	}

	return err
}
