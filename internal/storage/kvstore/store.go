// Package kvstore is the storage of issues, events, logs and the ingest
// inbox on the key-value layer (internal/storage/kv; docs/storage-plan.md).
// Its records and their keys are declared in schema.go. Until every part of
// the storage moves here, the methods not written yet are those of the
// store.Store it is given (users, projects, notifications, agents and
// retention); it does not know which store that is.
package kvstore

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// Store is the database. Issues are also kept in memory (issues.go), and so
// are the time marks (marks.go).
type Store struct {
	model.Store

	db *kv.DB
	collections

	// inboxBytes is the size of the entries waiting in the inbox.
	inboxBytes atomic.Int64

	// table is every issue, in memory (issues.go); guarded by issuesMutex.
	issuesMutex sync.RWMutex
	table       *issueTable

	marksMutex sync.RWMutex
	marks      []timeMark

	// now is the clock: time.Now, or a test's.
	now func() time.Time

	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
}

var _ model.Store = (*Store)(nil)

// Open opens (or creates) the database in dir; rest serves the parts not
// written here yet.
func Open(dir string, rest model.Store) (*Store, error) {
	engine, err := kv.Open(dir)

	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	return open(engine, rest)
}

// OpenMemory opens a store kept in memory, for tests.
func OpenMemory(rest model.Store) (*Store, error) { return open(kv.OpenMemory(), rest) }

func open(engine kv.Engine, rest model.Store) (*Store, error) {
	schema := kv.NewSchema()
	database := &Store{Store: rest, collections: defineCollections(schema), now: time.Now}
	db, err := kv.OpenDB(engine, schema)

	if err == nil {
		database.db = db
		err = errors.Join(database.loadIssues(), database.loadInboxSize(), database.loadMarks())
	}

	if err != nil {
		_ = engine.Close()

		return nil, err
	}

	return database, nil
}

// Close closes the database and the store of the other parts; calling it
// again returns the first result.
func (database *Store) Close() error {
	database.closeOnce.Do(func() {
		database.closed.Store(true)
		database.closeErr = database.db.Close()

		if database.Store != nil {
			database.closeErr = errors.Join(database.closeErr, database.Store.Close())
		}
	})

	return database.closeErr
}

func (database *Store) Ping(context.Context) error { return nil }

// DatabaseSize is what this store's engine takes on disk.
func (database *Store) DatabaseSize(context.Context) (int64, error) { return database.db.Size() }

// SetClock replaces the store's clock (tests).
func (database *Store) SetClock(now func() time.Time) { database.now = now }

// levelRank is a level's place in config.LogLevels, lowest first: keys
// lead with it, so lower levels sort first and the cleanup cuts them first.
// Event levels count as their log name (warning is warn).
func levelRank(level string) byte {
	return byte(slices.Index(config.LogLevels, config.LogLevel(level)))
}

// allRanks are the ranks a list of every level merges.
var allRanks = func() []byte {
	ranks := make([]byte, len(config.LogLevels))

	for index := range ranks {
		ranks[index] = byte(index)
	}

	return ranks
}()

const day = 24 * time.Hour

// dayOf is the number of days since 1970-01-01 (UTC).
func dayOf(at time.Time) int32 { return int32(at.UTC().Unix() / int64(day/time.Second)) }

func dayStart(number int32) time.Time {
	return time.Unix(int64(number)*int64(day/time.Second), 0).UTC()
}

// notFound maps the layer's "not found" to the store's.
func notFound(err error) error {
	if errors.Is(err, kv.ErrNotFound) {
		return model.ErrNotFound
	}

	return err
}
