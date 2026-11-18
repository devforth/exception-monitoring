package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

const projectColumns = `id, name, public_key, event_levels, log_levels, color, created_at`

func scanProject(row interface{ Scan(...any) error }) (Project, error) {
	var (
		project                Project
		eventLevels, logLevels sql.NullString
		createdAt              string
	)

	if err := row.Scan(&project.ID, &project.Name, &project.PublicKey, &eventLevels, &logLevels, &project.Color, &createdAt); err != nil {
		return project, err
	}

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
		INSERT INTO projects (name, public_key, event_levels, log_levels, color, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		project.Name, project.PublicKey, levelsOrNull(project.EventLevels), levelsOrNull(project.LogLevels), project.Color, formatTime(now), formatTime(now))

	if err != nil {
		return 0, conflictError(err)
	}

	newID, err := result.LastInsertId()

	return int(newID), err
}

// UpdateProject saves the settings of a project.
func (store *Store) UpdateProject(ctx context.Context, project Project, now time.Time) error {
	result, err := store.DB.ExecContext(ctx, `
		UPDATE projects SET name = ?, public_key = ?, event_levels = ?, log_levels = ?, color = ?, updated_at = ?
		WHERE id = ?`,
		project.Name, project.PublicKey, levelsOrNull(project.EventLevels), levelsOrNull(project.LogLevels), project.Color, formatTime(now), project.ID)

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
// them; settings from the UI stay). A name or key taken by another
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
