// Package leveldb adapts goleveldb to kv.Engine; no other code imports goleveldb.
package leveldb

import (
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/storage"
	"github.com/syndtr/goleveldb/leveldb/util"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
)

// Name names the store; its data lives in DATA_DIR/<Name>.
const Name = "leveldb"

func options() *opt.Options {
	return &opt.Options{
		BlockCacheCapacity:  32 << 20,
		WriteBuffer:         16 << 20,
		CompactionTableSize: 8 << 20,
		// Most unique-key lookups miss.
		Filter:      filter.NewBloomFilter(10),
		Compression: opt.SnappyCompression,
		// Higher level-0 limits than the defaults, so ingest bursts are not held back.
		CompactionL0Trigger:    8,
		WriteL0SlowdownTrigger: 16,
		WriteL0PauseTrigger:    32,
		DisableSeeksCompaction: true,
	}
}

// Open opens (or creates) the database in dir.
func Open(dir string) (kv.Engine, error) { return openFile(dir, options()) }

// OpenReadOnly opens the database in dir without writing to it; a writer holding it gives kv.ErrLocked.
func OpenReadOnly(dir string) (kv.Engine, error) {
	settings := options()
	settings.ReadOnly = true

	return openFile(dir, settings)
}

func openFile(dir string, settings *opt.Options) (kv.Engine, error) {
	db, err := leveldb.OpenFile(dir, settings)

	if errors.Is(err, syscall.EWOULDBLOCK) {
		return nil, fmt.Errorf("%w: %w", kv.ErrLocked, err)
	}

	if err != nil {
		return nil, err
	}

	return &levelEngine{levelReader: levelReader{db: db}, db: db, dir: dir}, nil
}

// OpenMemory opens a database kept in memory, for tests.
func OpenMemory() kv.Engine {
	db, err := leveldb.Open(storage.NewMemStorage(), options())

	if err != nil {
		panic(err) // a memory storage cannot fail to open
	}

	return &levelEngine{levelReader: levelReader{db: db}, db: db}
}

// levelGetter is what a goleveldb DB and its snapshot share.
type levelGetter interface {
	Get(key []byte, ro *opt.ReadOptions) ([]byte, error)
	NewIterator(slice *util.Range, ro *opt.ReadOptions) iterator.Iterator
}

type levelReader struct {
	db      levelGetter
	options *opt.ReadOptions
}

func (reader levelReader) Get(key []byte) ([]byte, error) {
	value, err := reader.db.Get(key, reader.options)

	if errors.Is(err, leveldb.ErrNotFound) {
		return nil, kv.ErrNotFound
	}

	return value, err
}

func (reader levelReader) Iterate(lower, upper []byte) kv.Iterator {
	return levelIterator{reader.db.NewIterator(&util.Range{Start: lower, Limit: upper}, reader.options)}
}

type levelIterator struct{ iterator.Iterator }

func (it levelIterator) Close() error {
	it.Release()

	return it.Error()
}

type levelEngine struct {
	levelReader
	db  *leveldb.DB
	dir string // empty in memory
}

func (engine *levelEngine) NewWriteBatch() kv.WriteBatch { return new(leveldb.Batch) }

func (engine *levelEngine) Write(batch *kv.Batch, sync bool) error {
	if batch.Len() == 0 {
		return nil
	}

	return engine.db.Write(batch.Native().(*leveldb.Batch), &opt.WriteOptions{Sync: sync})
}

func (engine *levelEngine) Snapshot() (kv.Snapshot, error) {
	snapshot, err := engine.db.GetSnapshot()

	if err != nil {
		return nil, err
	}

	// Snapshots serve backups, whose reads should not evict the blocks ingest uses.
	reader := levelReader{db: snapshot, options: &opt.ReadOptions{DontFillCache: true}}

	return levelSnapshot{levelReader: reader, snapshot: snapshot}, nil
}

type levelSnapshot struct {
	levelReader
	snapshot *leveldb.Snapshot
}

func (snapshot levelSnapshot) Release() { snapshot.snapshot.Release() }

func (engine *levelEngine) CompactRange(lower, upper []byte) error {
	return engine.db.CompactRange(util.Range{Start: lower, Limit: upper})
}

// Size sums the database files in use; in memory, the keys and values.
func (engine *levelEngine) Size() (int64, error) {
	if engine.dir == "" {
		var total int64

		iterator := engine.Iterate(nil, nil)

		for valid := iterator.First(); valid; valid = iterator.Next() {
			total += int64(len(iterator.Key()) + len(iterator.Value()))
		}

		return total, iterator.Close()
	}

	live, err := engine.liveTables()

	if err != nil {
		return 0, err
	}

	var total int64

	err = filepath.WalkDir(engine.dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A compaction may delete a file while walking.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return err
		}

		if entry.IsDir() {
			return nil
		}

		// A table a compaction replaced stays on disk until goleveldb's next version change.
		if number, ok := tableNumber(entry.Name()); ok && !live[number] {
			return nil
		}

		info, err := entry.Info()

		if err == nil {
			total += info.Size()
		} else if !errors.Is(err, fs.ErrNotExist) {
			return err
		}

		return nil
	})

	return total, err
}

// liveTables lists the numbers of the tables in use, from "N:size[...]" lines.
func (engine *levelEngine) liveTables() (map[int64]bool, error) {
	tables, err := engine.db.GetProperty("leveldb.sstables")

	if err != nil {
		return nil, err
	}

	live := map[int64]bool{}

	for line := range strings.Lines(tables) {
		var number, size int64

		if count, _ := fmt.Sscanf(line, "%d:%d", &number, &size); count == 2 {
			live[number] = true
		}
	}

	return live, nil
}

// tableNumber reads "000123.ldb" (or the older ".sst").
func tableNumber(name string) (int64, bool) {
	extension := filepath.Ext(name)

	if extension != ".ldb" && extension != ".sst" {
		return 0, false
	}

	number, err := strconv.ParseInt(strings.TrimSuffix(name, extension), 10, 64)

	return number, err == nil
}

func (engine *levelEngine) Close() error { return engine.db.Close() }
