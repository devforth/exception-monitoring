package database

import (
	"cmp"
	"context"
	"crypto/subtle"
	"slices"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// destinationRecord is a destination with its pending email code.
type destinationRecord struct {
	model.Destination
	CodeHash      string    `json:"code_hash,omitempty"`
	CodeExpiresAt time.Time `json:"code_expires_at"`
	CodeAttempts  int       `json:"code_attempts,omitempty"`
}

// connectionRecord is a connection without the fields read from its destination.
type connectionRecord struct {
	ID            string    `json:"id"`
	Name          string    `json:"name"`
	DestinationID string    `json:"destination_id"`
	AllProjects   bool      `json:"all_projects"`
	ProjectIDs    []int     `json:"project_ids,omitempty"`
	Levels        []string  `json:"levels,omitempty"`
	Active        bool      `json:"active"`
	HideIssueLink bool      `json:"hide_issue_link"`
	CreatedAt     time.Time `json:"created_at"`
}

func (database *Store) destination(reader kv.Reader, id string) (destinationRecord, error) {
	destination, err := database.destinations.Get(reader, kv.K(kv.Text(id)))

	return destination, notFound(err)
}

func (database *Store) CreateDestination(_ context.Context, destination model.Destination, now time.Time) (string, error) {
	destination.Address, destination.CreatedAt = strings.ToLower(destination.Address), now.UTC()

	err := database.db.Update(true, func(batch *kv.Batch) error {
		var err error

		if destination.ID, err = freeID(batch, database.destinations); err != nil {
			return err
		}

		return database.destinations.Insert(batch, destinationRecord{Destination: destination})
	})

	return destination.ID, err
}

func (database *Store) GetDestination(_ context.Context, id string) (model.Destination, error) {
	destination, err := database.destination(database.db, id)

	return destination.Destination, err
}

// FindEmailDestination returns the destination of an address (in any case), a confirmed one if there is any.
func (database *Store) FindEmailDestination(_ context.Context, address string) (model.Destination, error) {
	destination, err := database.destinations.By("email", kv.Text(strings.ToLower(address))).First(database.db)

	return destination.Record.Destination, notFound(err)
}

// changeDestination rewrites a destination in one batch.
func (database *Store) changeDestination(id string, change func(destination *destinationRecord) error) error {
	return database.db.Update(true, func(batch *kv.Batch) error {
		destination, err := database.destination(batch, id)

		if err != nil {
			return err
		}

		if err := change(&destination); err != nil {
			return err
		}

		return database.destinations.Save(batch, destination)
	})
}

// UpdateDestinationSecret replaces the encrypted tokens of a chat.
func (database *Store) UpdateDestinationSecret(_ context.Context, id string, secret string) error {
	return database.changeDestination(id, func(destination *destinationRecord) error {
		destination.Secret = secret

		return nil
	})
}

// SetVerificationCode stores the hash of a newly sent code, resets the attempts and remembers when it was sent.
func (database *Store) SetVerificationCode(_ context.Context, destinationID string, codeHash string, expiresAt, now time.Time) error {
	return database.changeDestination(destinationID, func(destination *destinationRecord) error {
		destination.CodeHash, destination.CodeExpiresAt, destination.CodeAttempts = codeHash, expiresAt.UTC(), 0
		destination.VerifySentAt = now.UTC()

		return nil
	})
}

// CheckVerificationCode compares the hash of an entered code. A match confirms
// the destination and clears the code; every check counts as an attempt.
func (database *Store) CheckVerificationCode(_ context.Context, destinationID string, codeHash string, now time.Time) (model.VerifyResult, error) {
	result := model.VerifyWrong

	err := database.db.Update(true, func(batch *kv.Batch) error {
		destination, err := database.destination(batch, destinationID)

		switch {
		case err != nil:
			return err
		case destination.CodeHash == "" || !now.Before(destination.CodeExpiresAt):
			result = model.VerifyExpired

			return nil
		case destination.CodeAttempts >= model.MaxVerifyAttempts:
			result = model.VerifyTooManyTries

			return nil
		}

		destination.CodeAttempts++

		if subtle.ConstantTimeCompare([]byte(destination.CodeHash), []byte(codeHash)) == 1 {
			result = model.VerifyOK
			destination.Verified, destination.VerifiedAt = true, now.UTC()
			destination.CodeHash, destination.CodeExpiresAt, destination.CodeAttempts = "", time.Time{}, 0
		}

		return database.destinations.Save(batch, destination)
	})

	return result, err
}

// connection fills a connection with the fields of its destination.
func (database *Store) connection(reader kv.Reader, record connectionRecord) (model.Connection, error) {
	destination, err := database.destination(reader, record.DestinationID)

	if err != nil {
		return model.Connection{}, err
	}

	return model.Connection{
		ID: record.ID, Name: record.Name, DestinationID: record.DestinationID, AllProjects: record.AllProjects,
		ProjectIDs: record.ProjectIDs, Levels: record.Levels, Active: record.Active, HideIssueLink: record.HideIssueLink,
		CreatedAt: record.CreatedAt,
		Type:      destination.Type, Address: destination.Address, Secret: destination.Secret, Verified: destination.Verified,
	}, nil
}

// ListConnections returns every connection with its projects, oldest first.
func (database *Store) ListConnections(context.Context) ([]model.Connection, error) {
	records, err := database.connections.Primary().All(database.db)

	if err != nil {
		return nil, err
	}

	slices.SortFunc(records, func(a, b connectionRecord) int {
		return cmp.Or(a.CreatedAt.Compare(b.CreatedAt), strings.Compare(a.ID, b.ID))
	})

	connections := make([]model.Connection, 0, len(records))

	for _, record := range records {
		connection, err := database.connection(database.db, record)

		if err != nil {
			return nil, err
		}

		connections = append(connections, connection)
	}

	return connections, nil
}

func (database *Store) GetConnection(_ context.Context, id string) (model.Connection, error) {
	record, err := database.connections.Get(database.db, kv.K(kv.Text(id)))

	if err != nil {
		return model.Connection{}, notFound(err)
	}

	return database.connection(database.db, record)
}

// connectionRecordOf keeps what a connection stores itself; "all projects" needs no list.
func connectionRecordOf(connection model.Connection) connectionRecord {
	record := connectionRecord{
		ID: connection.ID, Name: connection.Name, DestinationID: connection.DestinationID, AllProjects: connection.AllProjects,
		Levels: connection.Levels, Active: connection.Active, HideIssueLink: connection.HideIssueLink, CreatedAt: connection.CreatedAt,
	}

	if !connection.AllProjects {
		record.ProjectIDs = slices.Compact(slices.Sorted(slices.Values(connection.ProjectIDs)))
	}

	return record
}

// requireVerified: a connection may exist (and be active) only to a confirmed destination (chats are confirmed when made).
func (database *Store) requireVerified(reader kv.Reader, destinationID string) error {
	destination, err := database.destination(reader, destinationID)

	if err == nil && !destination.Verified {
		err = model.ErrNotVerified
	}

	return err
}

// CreateConnection stores a new connection to a confirmed destination.
func (database *Store) CreateConnection(_ context.Context, connection model.Connection, now time.Time) (string, error) {
	connection.CreatedAt = now.UTC()

	err := database.db.Update(true, func(batch *kv.Batch) error {
		if err := database.requireVerified(batch, connection.DestinationID); err != nil {
			return err
		}

		var err error

		if connection.ID, err = freeID(batch, database.connections); err != nil {
			return err
		}

		return database.connections.Insert(batch, connectionRecordOf(connection))
	})

	return connection.ID, err
}

// UpdateConnection saves the name, projects, levels and active; an active
// connection needs a confirmed destination.
func (database *Store) UpdateConnection(_ context.Context, connection model.Connection) error {
	return database.db.Update(true, func(batch *kv.Batch) error {
		stored, err := database.connections.Get(batch, kv.K(kv.Text(connection.ID)))

		if err != nil {
			return notFound(err)
		}

		if connection.Active {
			if err := database.requireVerified(batch, stored.DestinationID); err != nil {
				return err
			}
		}

		connection.DestinationID, connection.CreatedAt = stored.DestinationID, stored.CreatedAt

		return database.connections.Save(batch, connectionRecordOf(connection))
	})
}

// DeleteConnection removes the connection and its queued messages. The
// destination stays, keeping an email address confirmed.
func (database *Store) DeleteConnection(_ context.Context, id string) error {
	return database.db.Update(true, func(batch *kv.Batch) error {
		connection, err := database.connections.Get(batch, kv.K(kv.Text(id)))

		if err != nil {
			return notFound(err)
		}

		if err := database.connections.Delete(batch, connection); err != nil {
			return err
		}

		return database.outbox.By("channel", kv.Text(id)).Each(batch, func(entry kv.Entry[outboxRecord]) (bool, error) {
			database.outbox.DeleteEntry(batch, entry)

			return true, nil
		})
	})
}

// dropProjectFromConnections removes a deleted project from the connections that list it.
func (database *Store) dropProjectFromConnections(batch *kv.Batch, projectID int) error {
	return database.connections.Primary().Each(batch, func(entry kv.Entry[connectionRecord]) (bool, error) {
		if !slices.Contains(entry.Record.ProjectIDs, projectID) {
			return true, nil
		}

		connection := entry.Record
		connection.ProjectIDs = slices.DeleteFunc(slices.Clone(connection.ProjectIDs), func(id int) bool { return id == projectID })

		return true, database.connections.Save(batch, connection)
	})
}
