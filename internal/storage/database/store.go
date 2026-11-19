// Package database is the store (store.Store) on the kv layer: every collection is declared in schema.go.
package database

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

// Store is the database; issues and time marks are also kept in memory.
type Store struct {
	db      *kv.DB
	dataDir string // empty in memory
	collections
	formatKey kv.Key

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
	// lastAppend is when the newest envelope came (or the store opened), in Unix nanoseconds.
	lastAppend atomic.Int64
	// backups counts running backups: their snapshot keeps deleted records on disk.
	backups atomic.Int32

	now func() time.Time

	closeOnce sync.Once
	closeErr  error
	closed    atomic.Bool
}

var _ model.Store = (*Store)(nil)

// Open opens the database in dataDir.
func Open(dataDir string) (*Store, error) {
	location := Location(dataDir)
	engine, err := openEngine(location)

	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}

	database, err := open(engine)

	if errors.Is(err, errFormat) {
		return nil, fmt.Errorf("%w: move %s away to start over", err, location)
	}

	if err != nil {
		return nil, err
	}

	database.dataDir = dataDir

	return database, nil
}

// errFormat: the database was written in another format.
var errFormat = errors.New("the database has another format")

// OpenMemory opens a store kept in memory, for tests.
func OpenMemory() (*Store, error) { return open(openMemoryEngine()) }

// storeFormat is the version of what this package writes. A database of
// another format is refused at start rather than misread; there are no
// migrations yet, so a change of format means starting over.
const storeFormat = "1"

func open(engine kv.Engine) (*Store, error) {
	schema := kv.NewSchema()
	formatKey := schema.Reserve(formatName)
	database := &Store{collections: defineCollections(schema), formatKey: formatKey, now: time.Now, messageStart: map[string]uint64{},
		cleanupWake: make(chan struct{}, 1)}
	database.lastAppend.Store(time.Now().UnixNano())

	db, err := kv.OpenDB(engine, schema)

	if err == nil {
		database.db = db
		err = database.checkFormat(formatKey)
	}

	if err == nil {
		err = errors.Join(database.loadIssues(), database.loadInboxSize(), database.loadMarks())
	}

	if err != nil {
		_ = engine.Close()

		return nil, err
	}

	return database, nil
}

// checkFormat writes the format into a new database and refuses one written in another.
func (database *Store) checkFormat(key kv.Key) error {
	stored, err := database.db.Get(key)

	switch {
	case err == nil:
		return sameFormat(stored)
	case !errors.Is(err, kv.ErrNotFound):
		return err
	}

	iterator := database.db.Iterate(nil, nil)
	used := iterator.First()

	if err := iterator.Close(); err != nil {
		return err
	}

	if used {
		return fmt.Errorf("%w: written by a development version before format %s", errFormat, storeFormat)
	}

	return database.db.Update(true, func(batch *kv.Batch) error {
		batch.Put(key, []byte(storeFormat))

		return nil
	})
}

// formatName reserves the key of the format.
const formatName = "format"

// sameFormat refuses a format other than storeFormat.
func sameFormat(stored []byte) error {
	if string(stored) != storeFormat {
		return fmt.Errorf("%w: %s, this version reads %s", errFormat, stored, storeFormat)
	}

	return nil
}

// readFormat checks the format of a database opened only to read.
func readFormat(reader kv.Reader) error {
	stored, err := reader.Get(kv.NewSchema().Reserve(formatName))

	switch {
	case errors.Is(err, kv.ErrNotFound):
		return fmt.Errorf("%w: none recorded", errFormat)
	case err != nil:
		return err
	}

	return sameFormat(stored)
}

// Close closes the database; calling it again returns the first result.
func (database *Store) Close() error {
	database.closeOnce.Do(func() {
		database.closed.Store(true)
		database.closeErr = database.db.Close()
	})

	return database.closeErr
}

// Ping reads the format key, so a closed store or a failing disk answers with an error.
func (database *Store) Ping(context.Context) error {
	_, err := database.db.Get(database.formatKey)

	return err
}

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
