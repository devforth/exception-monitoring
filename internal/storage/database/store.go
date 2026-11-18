// Package database stores issues, events, logs and the ingest inbox on the
// kv layer; everything else passes to the store.Store it is given.
package database

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// Store is the database; issues and time marks are also kept in memory.
type Store struct {
	model.Store

	db *kv.DB
	collections

	inboxBytes atomic.Int64

	// table holds every issue; guarded by issuesMutex.
	issuesMutex sync.RWMutex
	table       *issueTable

	marksMutex sync.RWMutex
	marks      []timeMark

	// messageStart is the oldest stored sequence per log message; used only inside updates.
	messageStart map[string]uint64

	// checkedAt is Written at the last wake.
	cleanupWake chan struct{}
	checkedAt   atomic.Int64

	now func() time.Time

	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
}

var _ model.Store = (*Store)(nil)

// Open opens the database in dataDir; rest serves the parts not stored here.
func Open(dataDir string, rest model.Store) (*Store, error) {
	engine, err := openEngine(filepath.Join(dataDir, engineName))

	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	return open(engine, rest)
}

// OpenMemory opens a store kept in memory, for tests.
func OpenMemory(rest model.Store) (*Store, error) { return open(openMemoryEngine(), rest) }

func open(engine kv.Engine, rest model.Store) (*Store, error) {
	schema := kv.NewSchema()
	database := &Store{Store: rest, collections: defineCollections(schema), now: time.Now, messageStart: map[string]uint64{},
		cleanupWake: make(chan struct{}, 1)}
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

// Close closes the database and rest; calling it again returns the first result.
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

// DatabaseSize returns the engine's size on disk.
func (database *Store) DatabaseSize(context.Context) (int64, error) { return database.db.Size() }

// SetClock replaces the store's clock (tests).
func (database *Store) SetClock(now func() time.Time) { database.now = now }

// levelRank returns a level's place in config.LogLevels, so lower levels sort first.
func levelRank(level string) byte {
	return byte(slices.Index(config.LogLevels, config.LogLevel(level)))
}

var allRanks = func() []byte {
	ranks := make([]byte, len(config.LogLevels))

	for index := range ranks {
		ranks[index] = byte(index)
	}

	return ranks
}()

const day = 24 * time.Hour

// dayOf returns the number of days since 1970-01-01 (UTC).
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
