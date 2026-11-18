package kv

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"

	"github.com/syndtr/goleveldb/leveldb"
	"github.com/syndtr/goleveldb/leveldb/filter"
	"github.com/syndtr/goleveldb/leveldb/iterator"
	"github.com/syndtr/goleveldb/leveldb/opt"
	"github.com/syndtr/goleveldb/leveldb/storage"
	"github.com/syndtr/goleveldb/leveldb/util"
)

// EngineName names the store this binary is built with; the database lives
// in DATA_DIR/<EngineName>.
const EngineName = "leveldb"

// options keep the files readable by LevelDB C++ too: Snappy only (its zstd
// is unknown to goleveldb) and the default comparator.
func options() *opt.Options {
	o := &opt.Options{
		BlockCacheCapacity: 32 << 20,
		WriteBuffer:        16 << 20,
		// 8 MB tables: about 250 files for 2 GB instead of 1000.
		CompactionTableSize: 8 << 20,
		// Most lookups by a unique key (event_id of a new event) miss.
		Filter:      filter.NewBloomFilter(10),
		Compression: opt.SnappyCompression,
		// goleveldb compacts in one thread. With its defaults (slow down
		// writes at 8 files in level 0, stop them at 12) a burst of ingest
		// held writes back 5.5 s in all, the inbox's included, and SDKs
		// waited up to 1.2 s; with these, 0.7 s and under 0.3 s. Reads may
		// look into a few more level-0 files meanwhile. Compactions that
		// reads would trigger are off: the writes keep compaction busy enough.
		CompactionL0Trigger:    8,
		WriteL0SlowdownTrigger: 16,
		WriteL0PauseTrigger:    32,
		DisableSeeksCompaction: true,
	}

	if os.Getenv("KV_NOPOOL") != "" {
		o.DisableBufferPool = true
	}

	if os.Getenv("KV_WB8") != "" {
		o.WriteBuffer = 8 << 20
	}

	return o
}

// Open opens (or creates) the database in dir.
func Open(dir string) (Engine, error) {
	db, err := leveldb.OpenFile(dir, options())

	if err != nil {
		return nil, err
	}

	return &levelEngine{levelReader: levelReader{db: db}, db: db, dir: dir}, nil
}

// OpenMemory opens a database kept in memory, for tests.
func OpenMemory() Engine {
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

type levelReader struct{ db levelGetter }

func (reader levelReader) Get(key []byte) ([]byte, error) {
	value, err := reader.db.Get(key, nil)

	if errors.Is(err, leveldb.ErrNotFound) {
		return nil, ErrNotFound
	}

	return value, err
}

func (reader levelReader) Iterate(lower, upper []byte) Iterator {
	return levelIterator{reader.db.NewIterator(&util.Range{Start: lower, Limit: upper}, nil)}
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

func (engine *levelEngine) NewWriteBatch() WriteBatch { return new(leveldb.Batch) }

func (engine *levelEngine) Write(batch *Batch, sync bool) error {
	if batch.Len() == 0 {
		return nil
	}

	return engine.db.Write(batch.native.(*leveldb.Batch), &opt.WriteOptions{Sync: sync})
}

func (engine *levelEngine) Snapshot() (Snapshot, error) {
	snapshot, err := engine.db.GetSnapshot()

	if err != nil {
		return nil, err
	}

	return levelSnapshot{levelReader: levelReader{db: snapshot}, snapshot: snapshot}, nil
}

type levelSnapshot struct {
	levelReader
	snapshot *leveldb.Snapshot
}

func (snapshot levelSnapshot) Release() { snapshot.snapshot.Release() }

func (engine *levelEngine) CompactRange(lower, upper []byte) error {
	return engine.db.CompactRange(util.Range{Start: lower, Limit: upper})
}

// Size sums the files of the database; in memory (tests), the keys and
// values, since what is not flushed to a table yet has no file size.
func (engine *levelEngine) Size() (int64, error) {
	if engine.dir == "" {
		var total int64

		iterator := engine.Iterate(nil, nil)

		for valid := iterator.First(); valid; valid = iterator.Next() {
			total += int64(len(iterator.Key()) + len(iterator.Value()))
		}

		return total, iterator.Close()
	}

	var total int64

	err := filepath.WalkDir(engine.dir, func(_ string, entry fs.DirEntry, err error) error {
		if err != nil {
			// A file deleted by a compaction while walking is simply gone.
			if errors.Is(err, fs.ErrNotExist) {
				return nil
			}

			return err
		}

		if entry.IsDir() {
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

func (engine *levelEngine) Close() error { return engine.db.Close() }
