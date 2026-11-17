package sqlite

import (
	"context"
	"crypto/subtle"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Destinations.

const destinationColumns = `id, type, address, secret, verified, verified_at, verify_sent_at, created_at`

func scanDestination(row scanner) (Destination, error) {
	var (
		destination                       Destination
		verifiedAt, verifySentAt, created string
	)

	err := row.Scan(&destination.ID, &destination.Type, &destination.Address, &destination.Secret,
		&destination.Verified, &verifiedAt, &verifySentAt, &created)

	if errors.Is(err, sql.ErrNoRows) {
		return destination, ErrNotFound
	}

	destination.VerifiedAt, destination.VerifySentAt, destination.CreatedAt = parseTime(verifiedAt), parseTime(verifySentAt), parseTime(created)

	return destination, err
}

func (store *Store) CreateDestination(ctx context.Context, destination Destination, now time.Time) (int64, error) {
	result, err := store.DB.ExecContext(ctx, `
		INSERT INTO notification_destinations (type, address, secret, verified, verified_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		destination.Type, strings.ToLower(destination.Address), destination.Secret, destination.Verified,
		formatOptionalTime(destination.VerifiedAt), formatTime(now), formatTime(now))

	if err != nil {
		return 0, err
	}

	return result.LastInsertId()
}

func (store *Store) GetDestination(ctx context.Context, id int64) (Destination, error) {
	return scanDestination(store.DB.QueryRowContext(ctx, `SELECT `+destinationColumns+` FROM notification_destinations WHERE id = ?`, id))
}

// FindEmailDestination returns the destination of an address (case-insensitive),
// a confirmed one if there is any.
func (store *Store) FindEmailDestination(ctx context.Context, address string) (Destination, error) {
	return scanDestination(store.DB.QueryRowContext(ctx, `
		SELECT `+destinationColumns+` FROM notification_destinations
		WHERE type = 'email' AND address = ?
		ORDER BY verified DESC, id LIMIT 1`, strings.ToLower(address)))
}

// UpdateDestinationSecret replaces the encrypted tokens of a chat.
func (store *Store) UpdateDestinationSecret(ctx context.Context, id int64, secret string, now time.Time) error {
	_, err := store.DB.ExecContext(ctx,
		`UPDATE notification_destinations SET secret = ?, updated_at = ? WHERE id = ?`, secret, formatTime(now), id)

	return err
}

// Email verification.

// SetVerificationCode stores the hash of a newly sent code, resets the
// attempts and remembers when it was sent.
func (store *Store) SetVerificationCode(ctx context.Context, destinationID int64, codeHash string, expiresAt, now time.Time) error {
	result, err := store.DB.ExecContext(ctx, `
		UPDATE notification_destinations
		SET verify_code_hash = ?, verify_expires_at = ?, verify_attempts = 0, verify_sent_at = ?, updated_at = ?
		WHERE id = ?`, codeHash, formatTime(expiresAt), formatTime(now), formatTime(now), destinationID)

	if err != nil {
		return err
	}

	if count, err := result.RowsAffected(); err == nil && count == 0 {
		return ErrNotFound
	}

	return nil
}

// CheckVerificationCode compares the hash of an entered code. A match marks
// the destination confirmed (with the date) and clears the code; every check
// counts as an attempt.
func (store *Store) CheckVerificationCode(ctx context.Context, destinationID int64, codeHash string, now time.Time) (VerifyResult, error) {
	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return VerifyWrong, err
	}

	defer transaction.Rollback()

	var (
		storedHash, expiresAt string
		attempts              int
	)

	err = transaction.QueryRowContext(ctx,
		`SELECT verify_code_hash, verify_expires_at, verify_attempts FROM notification_destinations WHERE id = ?`, destinationID).
		Scan(&storedHash, &expiresAt, &attempts)

	if errors.Is(err, sql.ErrNoRows) {
		return VerifyWrong, ErrNotFound
	}

	if err != nil {
		return VerifyWrong, err
	}

	switch {
	case storedHash == "" || !now.Before(parseTime(expiresAt)):
		return VerifyExpired, nil
	case attempts >= MaxVerifyAttempts:
		return VerifyTooManyTries, nil
	}

	result := VerifyWrong
	update := `UPDATE notification_destinations SET verify_attempts = verify_attempts + 1, updated_at = ? WHERE id = ?`
	args := []any{formatTime(now), destinationID}

	if subtle.ConstantTimeCompare([]byte(storedHash), []byte(codeHash)) == 1 {
		result = VerifyOK
		update = `UPDATE notification_destinations
			SET verified = 1, verified_at = ?, verify_code_hash = '', verify_expires_at = '', verify_attempts = 0, updated_at = ?
			WHERE id = ?`
		args = []any{formatTime(now), formatTime(now), destinationID}
	}

	if _, err := transaction.ExecContext(ctx, update, args...); err != nil {
		return VerifyWrong, err
	}

	return result, transaction.Commit()
}

// Connections.

const connectionSelect = `
	SELECT connections.id, connections.name, connections.destination_id, connections.all_projects, connections.levels,
		connections.active, connections.created_at, connections.hide_issue_link,
		destinations.type, destinations.address, destinations.secret, destinations.verified
	FROM notification_connections AS connections
	JOIN notification_destinations AS destinations ON destinations.id = connections.destination_id`

func scanConnection(row scanner) (Connection, error) {
	var (
		connection Connection
		levels     string
		createdAt  string
	)

	err := row.Scan(&connection.ID, &connection.Name, &connection.DestinationID, &connection.AllProjects, &levels,
		&connection.Active, &createdAt, &connection.HideIssueLink, &connection.Type, &connection.Address, &connection.Secret, &connection.Verified)

	if err != nil {
		return connection, err
	}

	if levels != "" {
		connection.Levels = strings.Split(levels, ",")
	}

	connection.CreatedAt = parseTime(createdAt)

	return connection, nil
}

// ListConnections returns every connection with its projects, oldest first.
func (store *Store) ListConnections(ctx context.Context) ([]Connection, error) {
	rows, err := store.DB.QueryContext(ctx, connectionSelect+` ORDER BY connections.id`)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	var connections []Connection

	for rows.Next() {
		connection, err := scanConnection(rows)

		if err != nil {
			return nil, err
		}

		connections = append(connections, connection)
	}

	if err := rows.Err(); err != nil {
		return nil, err
	}

	projects, err := store.connectionProjects(ctx)

	if err != nil {
		return nil, err
	}

	for index := range connections {
		connections[index].ProjectIDs = projects[connections[index].ID]
	}

	return connections, nil
}

func (store *Store) GetConnection(ctx context.Context, id int64) (Connection, error) {
	connection, err := scanConnection(store.DB.QueryRowContext(ctx, connectionSelect+` WHERE connections.id = ?`, id))

	if errors.Is(err, sql.ErrNoRows) {
		return connection, ErrNotFound
	}

	if err != nil {
		return connection, err
	}

	projects, err := store.connectionProjects(ctx)
	connection.ProjectIDs = projects[id]

	return connection, err
}

func (store *Store) connectionProjects(ctx context.Context) (map[int64][]int, error) {
	rows, err := store.DB.QueryContext(ctx, `SELECT connection_id, project_id FROM notification_connection_projects ORDER BY project_id`)

	if err != nil {
		return nil, err
	}

	defer rows.Close()

	projects := map[int64][]int{}

	for rows.Next() {
		var (
			connectionID int64
			projectID    int
		)

		if err := rows.Scan(&connectionID, &projectID); err != nil {
			return nil, err
		}

		projects[connectionID] = append(projects[connectionID], projectID)
	}

	return projects, rows.Err()
}

// destinationVerified reports whether a connection to the destination may
// exist (and be active): chats always, email addresses once confirmed.
func destinationVerified(ctx context.Context, transaction *sql.Tx, destinationID int64) (bool, error) {
	var verified bool

	err := transaction.QueryRowContext(ctx, `SELECT verified FROM notification_destinations WHERE id = ?`, destinationID).Scan(&verified)

	if errors.Is(err, sql.ErrNoRows) {
		return false, ErrNotFound
	}

	return verified, err
}

// CreateConnection stores a new connection to a confirmed destination.
func (store *Store) CreateConnection(ctx context.Context, connection Connection, now time.Time) (int64, error) {
	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return 0, err
	}

	defer transaction.Rollback()

	verified, err := destinationVerified(ctx, transaction, connection.DestinationID)

	if err != nil {
		return 0, err
	}

	if !verified {
		return 0, ErrNotVerified
	}

	result, err := transaction.ExecContext(ctx, `
		INSERT INTO notification_connections (name, destination_id, all_projects, levels, active, created_at, updated_at, hide_issue_link)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		connection.Name, connection.DestinationID, connection.AllProjects, strings.Join(connection.Levels, ","),
		connection.Active, formatTime(now), formatTime(now), connection.HideIssueLink)

	if err != nil {
		return 0, err
	}

	id, err := result.LastInsertId()

	if err != nil {
		return 0, err
	}

	if err := setConnectionProjects(ctx, transaction, id, connection); err != nil {
		return 0, err
	}

	return id, transaction.Commit()
}

// UpdateConnection saves the name, projects, levels and active. An active
// connection needs a confirmed destination.
func (store *Store) UpdateConnection(ctx context.Context, connection Connection, now time.Time) error {
	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return err
	}

	defer transaction.Rollback()

	if connection.Active {
		verified, err := destinationVerified(ctx, transaction, connection.DestinationID)

		if err != nil {
			return err
		}

		if !verified {
			return ErrNotVerified
		}
	}

	result, err := transaction.ExecContext(ctx, `
		UPDATE notification_connections SET name = ?, all_projects = ?, levels = ?, active = ?, updated_at = ?, hide_issue_link = ?
		WHERE id = ?`,
		connection.Name, connection.AllProjects, strings.Join(connection.Levels, ","), connection.Active, formatTime(now), connection.HideIssueLink, connection.ID)

	if err != nil {
		return err
	}

	if count, err := result.RowsAffected(); err == nil && count == 0 {
		return ErrNotFound
	}

	if err := setConnectionProjects(ctx, transaction, connection.ID, connection); err != nil {
		return err
	}

	return transaction.Commit()
}

func setConnectionProjects(ctx context.Context, transaction *sql.Tx, id int64, connection Connection) error {
	if _, err := transaction.ExecContext(ctx, `DELETE FROM notification_connection_projects WHERE connection_id = ?`, id); err != nil {
		return err
	}

	// "All projects" needs no list: it also covers projects added later.
	if connection.AllProjects {
		return nil
	}

	for _, projectID := range connection.ProjectIDs {
		if _, err := transaction.ExecContext(ctx,
			`INSERT OR IGNORE INTO notification_connection_projects (connection_id, project_id) VALUES (?, ?)`, id, projectID); err != nil {
			return fmt.Errorf("connection project: %w", err)
		}
	}

	return nil
}

// DeleteConnection removes the connection and its queued messages. The
// destination stays, keeping an email address confirmed.
func (store *Store) DeleteConnection(ctx context.Context, id int64) error {
	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return err
	}

	defer transaction.Rollback()

	result, err := transaction.ExecContext(ctx, `DELETE FROM notification_connections WHERE id = ?`, id)

	if err != nil {
		return err
	}

	if count, err := result.RowsAffected(); err == nil && count == 0 {
		return ErrNotFound
	}

	if _, err := transaction.ExecContext(ctx, `DELETE FROM notification_outbox WHERE channel = ?`, strconv.FormatInt(id, 10)); err != nil {
		return err
	}

	return transaction.Commit()
}

func formatOptionalTime(value time.Time) string {
	if value.IsZero() {
		return ""
	}

	return formatTime(value)
}
