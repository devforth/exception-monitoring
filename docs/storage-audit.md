# Embedded key-value stores compared: Pebble, LevelDB, goleveldb

Date: 2026-11-18.

Purpose: an independent comparison of three embedded LSM key-value stores by maintenance, adoption, features and performance. Activity and adoption data were collected on 17–18 November 2026. The measurements were made under the same conditions for all three.

## 1. What is compared

| | Pebble | LevelDB | goleveldb |
|---|---|---|---|
| Repository | [cockroachdb/pebble](https://github.com/cockroachdb/pebble) | [google/leveldb](https://github.com/google/leveldb) | [syndtr/goleveldb](https://github.com/syndtr/goleveldb) |
| Language | Go | C++ | Go (a port of LevelDB) |
| Latest version | v2.1.7 (2026-08-24) | release 1.23 (2021-02-23); `main` is newer | v1.0.0 (2019); master is newer |
| License | BSD-3-Clause | BSD-3-Clause | BSD-2-Clause |
| Code size (without tests) | ~137k lines | ~17.5k lines | ~18k lines |
| Developer | Cockroach Labs | Google | community (one main author) |
| Origin | grew out of golang/leveldb (license: "The LevelDB-Go Authors"), at first compatible with the RocksDB format; now its own way | the original implementation (2011) | a port of LevelDB to Go |

## 2. Maintenance

| | Pebble | LevelDB | goleveldb |
|---|---|---|---|
| Last commit | 2026 (100+ commits since the start of 2026) | 2026-11-12 | 2022-07-21 |
| Activity | steady development, regular releases | 13 commits in 2025–2026: build fixes, dependency bumps, an overflow fix, `Cache::SetCapacity` | none |
| Official position | active development | README: "receiving very limited maintenance", only fixes for critical bugs and changes Google's own clients need | — |
| New features | keep coming | zstd on `main`, in no release | none |

LevelDB from Go: there is no official Go binding. The known cgo binding, [jmhodges/levigo](https://github.com/jmhodges/levigo), was last updated on 2019-12-14.

## 3. Adoption

Download counts of Go modules are not published anywhere. The closest public measure is the number of open-source Go modules that import a package (pkg.go.dev, 2026-11-18). The C++ LevelDB library cannot be counted this way.

| Package | Importing modules |
|---|---|
| `syndtr/goleveldb/leveldb` | 5,827 |
| `cockroachdb/pebble` (v1) | 851 |
| `cockroachdb/pebble/v2` | 102 |
| `jmhodges/levigo` (LevelDB from Go through cgo) | 111 |

For comparison: `go.etcd.io/bbolt` 4,250, `dgraph-io/badger/v4` 1,018.

| | GitHub stars | Known users |
|---|---|---|
| LevelDB | 39.5k | Chromium (IndexedDB), Bitcoin Core (chain state) |
| goleveldb | 6.3k | go-ethereum (next to Pebble), many Go projects, among them Ethereum forks |
| Pebble | 6.1k | CockroachDB (its main storage engine), go-ethereum (`go.mod` requires `pebble/v2` next to goleveldb) |

Notes on these figures:
- The number of importers depends on the package's age: goleveldb exists since 2013, and Pebble v2 is recent.
- One importing module may be a small project or a system with thousands of installations.

## 4. Features

| | Pebble | LevelDB | goleveldb |
|---|---|---|---|
| Atomic write batches | yes | yes | yes |
| Synced writes, write-ahead log | yes | yes | yes |
| Snapshots, iterators | yes | yes | yes |
| Deleting a key range in one operation | yes (`DeleteRange`) | no | no |
| Compacting a range on demand | yes (`Compact`) | yes (`CompactRange`) | yes (`CompactRange`) |
| Concurrent compactions | yes | no (one background thread) | no |
| Bloom filters | yes | yes | yes |
| Compression | Snappy (default), zstd | Snappy (default); zstd on `main` only | Snappy |
| Metrics | detailed: disk usage split into live, obsolete and zombie files; levels, compactions, cache | properties (`leveldb.stats`, `leveldb.sstables`), approximate range sizes | properties similar to LevelDB |
| Format stability | major format versions with a migration (v1 → v2) | format stable since 2011 | compatible with the LevelDB format |
| Use from Go | native | only through cgo (static libstdc++, a C++ compiler for every target architecture, the cost of each Go → C call) | native |
| On an internal error | a Go panic with a stack trace | the process aborts | a Go panic with a stack trace |

## 5. Method

- **Machine:** Intel Core i7-12700H (20 threads), 37 GB RAM, Linux 7.0, Go 1.27.4.
- **Storage:**
  - **NVMe:** Samsung PM9A1, ext4 on LVM on dm-crypt. The main results.
  - **tmpfs:** memory. `fsync` costs nothing there, so it shows only the CPU part of each store's work.
- **Versions:**
  - Pebble v2.1.7;
  - goleveldb from master (`126854af`, 2022-07-21);
  - LevelDB from `main` (`29465588`, 2026-11-12), built Release with Snappy 1.2.2 and called through its official C API (`leveldb/c.h`) from cgo.
- **The same settings for all three:**
  - block cache 32 MB;
  - write buffer (memtable) 16 MB;
  - Snappy compression;
  - every write synced;
  - no bloom filters unless stated otherwise, which is the default of all three.
- **Data:** 1,000,000 records with time-ordered keys (12 bytes) and JSON values of about 300 bytes, like log lines.
- **Runs:** each test on a fresh directory, at least two runs. Ranges are shown.

**Test A: batch mode.** The same procedure for all:
1. write 1,000,000 records in synced batches of 1,000;
2. 200,000 random reads of existing keys;
3. a full scan with an iterator;
4. delete the oldest 30% key by key, in synced batches of 1,000;
5. compact the deleted range;
6. close and measure the directory.

**Test B: concurrent writes.** 50 goroutines, each writing 2,000 single synced records.

**Test C: range delete** (Pebble only, since the others have no such operation). Write 1,000,000 records and delete the same 30% with one `DeleteRange`.

## 6. Results

### 6.1. Batch mode on NVMe

| | Write | Random read | Full scan | Delete 30% key by key | Compact range |
|---|---|---|---|---|---|
| Pebble | **110–114k/s** | 34–38k/s | 0.95–1.13 s | **0.63–0.73 s** | 2.66–2.95 s |
| LevelDB | 76–77k/s | 60–69k/s | 1.18–1.23 s | 2.40–2.45 s | **0.68–0.76 s** |
| goleveldb | 79–81k/s | 60–66k/s | **0.89–1.05 s** | 2.52–2.58 s | 1.89–2.17 s |

Range delete in Pebble (test C): 0.002 s, then compaction 2.6–2.7 s.

**Space after closing:**

| | Data files (SST/LDB) | Directory in total | What else is there |
|---|---|---|---|
| Pebble | 89 MB | 145 MB | 55 MB of write-ahead log files: Pebble keeps old log files to reuse them |
| LevelDB | 85 MB | 85 MB | — |
| goleveldb | 93–99 MB | 93–99 MB | — |

**Peak process memory (RSS):** Pebble 60 MB, goleveldb 188–226 MB, LevelDB 310–345 MB.
- For LevelDB, RSS likely includes table files mapped into memory (mmap). The system can reclaim those pages, so this is not memory the library itself holds.
- For goleveldb, RSS includes the Go heap before garbage collection.

### 6.2. Reads with other settings (NVMe)

| | No filters, 32 MB cache | Bloom filter 10 bits/key, 32 MB cache | No filters, 256 MB cache (1 run) |
|---|---|---|---|
| Pebble | 34–38k/s | 25.5–26.2k/s | 67.6k/s |
| LevelDB | 60–69k/s | 61.6–63.2k/s | **108.8k/s** |
| goleveldb | 60–66k/s | 55.8–61.8k/s | 70.7k/s |

An extra check: Pebble's lag with a small cache is not caused by unfinished compaction.
- After the writes, level 0 of the LSM held 75 files in a single sublevel.
- A 15 s pause before reading did not change the result.

### 6.3. Concurrent synced writes

| | NVMe | p50 | p99 | Max |
|---|---|---|---|---|
| Pebble | 3,706–3,743/s | 13.2–13.3 ms | 18.1–18.3 ms | 37–44 ms |
| LevelDB | 2,766–3,737/s | 13.6–18.2 ms | 17.0–28.1 ms | 31–63 ms |
| goleveldb | 3,121–3,630/s | 13.8–14.5 ms | 17.1–28.3 ms | 29–48 ms |

| | tmpfs | p50 | p99 | Max |
|---|---|---|---|---|
| Pebble | **194–202k/s** | **62–64 µs** | 2.0 ms | 13–14 ms |
| LevelDB | 107–110k/s | 388–390 µs | 1.5–1.7 ms | 13–29 ms |
| goleveldb | 86–96k/s | 513–546 µs | **1.4–1.8 ms** | **4–6 ms** |

On NVMe the second run of LevelDB and goleveldb was slower than the first (disk noise), hence the wide ranges.

### 6.4. Batch mode on tmpfs

| | Write | Random read | Full scan | Delete 30% key by key | Compact |
|---|---|---|---|---|---|
| Pebble | 176–180k/s | 34k/s | 0.92–0.97 s | **0.10–0.12 s** | 2.23 s |
| LevelDB | 167–170k/s | **64–66k/s** | 1.28–1.33 s | 0.33–0.36 s | **0.58–0.59 s** |
| goleveldb | **194k/s** | 60–63k/s | 0.93–0.97 s | 0.37–0.48 s | 2.27–2.35 s |

## 7. Observations

1. **Batch writes on disk:** Pebble is about 40% faster than both LevelDBs. In memory (tmpfs) all three are close, with goleveldb slightly ahead. Pebble's lead on disk therefore comes from how it handles syncs and I/O, not from the CPU part of its work.
2. **Concurrent synced writes on disk:**
   - All three are bound by `fsync` (~13 ms on this device) and reach about the same throughput, since all of them group concurrent writes into one sync.
   - In memory, Pebble is twice as fast, with a 6–8× lower median latency.
   - goleveldb has the narrowest spread of latencies.
3. **Point reads:**
   - with a small cache, both LevelDBs are about 1.8× faster than Pebble;
   - Pebble's bloom filter did not help in this test, which read only existing keys;
   - with a cache larger than the data, LevelDB leads (~109k/s), while Pebble and goleveldb are level (~68–71k/s).
4. **Full scan:** Pebble and goleveldb are level; LevelDB is slightly slower.
5. **Deletes:** key by key, Pebble is 3.5–4× faster. Its range delete is nearly instant, and the others have no such operation.
6. **Compacting a given range:** fastest in LevelDB, slowest in Pebble.
7. **Disk space:** data files are nearly the same (85–99 MB). Pebble also keeps log files for reuse (55 MB in this test).
8. **Memory:** Pebble has the lowest peak RSS. The comparison is complicated by mmap in LevelDB and garbage collection in goleveldb.

## 8. Limits of these measurements

- **One machine, short runs** (seconds to half a minute), one process, one data size (~300 MB before compression).
- **The cost of `fsync` depends on the device:** dm-crypt and LVM add to it, and server SSDs with power-loss protection sync much faster.
- **A synthetic workload:** keys ordered by time, and only existing keys read.
- **Not tested:**
  - mixed reads and writes under sustained load;
  - data larger than the system's page cache;
  - behavior after a crash;
  - long-term degradation.
- **Uneven number of runs:** the reads with a 256 MB cache were run once.

## 9. Strengths and weaknesses

### Pebble

Strengths:
- active development and regular releases;
- the fastest batch writes and key-by-key deletes on disk;
- the lowest concurrent-write latency when the disk does not interfere;
- range deletes and concurrent compactions;
- detailed metrics;
- the lowest peak memory in these tests;
- native Go.

Weaknesses:
- point reads with a small cache about 1.8× slower than LevelDB;
- the slowest compaction of a given range;
- a large code base (~137k lines);
- developed for CockroachDB's needs; major versions change the format and the API (v1 → v2);
- keeps extra log files on disk;
- fewer open-source importers than goleveldb.

### LevelDB (C++)

Strengths:
- the most mature, with a format stable since 2011, widely proven (Chromium, Bitcoin Core);
- small code (~17.5k lines);
- the fastest point reads, especially with a large cache;
- the fastest compaction of a given range;
- the smallest data files in these tests.

Weaknesses:
- limited maintenance: last release in 2021, critical fixes only, new features (zstd) only on `main`;
- the slowest batch writes on disk in these tests;
- no range delete;
- one compaction thread;
- from Go only through cgo, with no maintained binding (the consequences are in section 4);
- the highest peak RSS (partly from mmap).

### goleveldb

Strengths:
- native Go, with the most open-source importers of the Go stores compared;
- the fastest batch writes in memory and the narrowest spread of concurrent-write latencies;
- small code (~18k lines);
- compatible with the LevelDB format.

Weaknesses:
- not maintained since 2022;
- on disk, batch writes on par with LevelDB and slower than Pebble;
- no range delete and no concurrent compactions;
- a high peak RSS from the Go heap.

### Format compatibility of LevelDB and goleveldb

Checked both ways, with and without bloom filters:
1. One implementation created a database with 100,000 records, deleted every tenth and compacted.
2. The other implementation added 100,000 more records the same way.
3. Each implementation then read all of them.

Both read every value correctly and saw exactly the 180,000 records left.

- **What the two share:** the comparator (`leveldb.BytewiseComparator`), Snappy's block code, and the formats of the tables, the log and the manifest.
- **Bloom filter names differ** (`leveldb.BuiltinBloomFilter` in goleveldb, `leveldb.BuiltinBloomFilter2` in LevelDB). Each ignores the other's filters until compaction rewrites the files, which costs speed but not correctness.
- **zstd blocks from LevelDB's `main` cannot be read by goleveldb.**
- **Not checked:** recovery after a crash, when data is only in the log.

## 10. What fits when

| Requirement | Fits better |
|---|---|
| Active maintenance and development | Pebble |
| Heavy writes, especially batches on disk | Pebble |
| Bulk deletion of key ranges (TTL, time-based cleanup) | Pebble |
| Mostly point reads, especially with a large cache | LevelDB |
| Minimal, stable code and a format unchanged for years | LevelDB |
| A C/C++ project | LevelDB |
| A Go project without cgo | Pebble or goleveldb; goleveldb only with its lack of maintenance in mind |
| Compatibility with existing LevelDB databases | LevelDB or goleveldb |

## 11. Reproducing

Building LevelDB with Snappy for a cgo call:

```sh
git clone --depth 1 https://github.com/google/leveldb.git
git clone --depth 1 --branch 1.2.2 https://github.com/google/snappy.git
cmake -S snappy -B build/snappy -DCMAKE_BUILD_TYPE=Release -DSNAPPY_BUILD_TESTS=OFF -DSNAPPY_BUILD_BENCHMARKS=OFF \
  -DBUILD_SHARED_LIBS=OFF -DCMAKE_POSITION_INDEPENDENT_CODE=ON -DCMAKE_INSTALL_PREFIX=$PWD/prefix
cmake --build build/snappy -j && cmake --install build/snappy
# LevelDB's own check misses a static Snappy, so it is switched on by hand.
cmake -S leveldb -B build/leveldb -DCMAKE_BUILD_TYPE=Release -DLEVELDB_BUILD_TESTS=OFF -DLEVELDB_BUILD_BENCHMARKS=OFF \
  -DBUILD_SHARED_LIBS=OFF -DCMAKE_POSITION_INDEPENDENT_CODE=ON -DHAVE_SNAPPY=1 \
  -DCMAKE_CXX_FLAGS="-I$PWD/prefix/include" -DCMAKE_INSTALL_PREFIX=$PWD/prefix
cmake --build build/leveldb -j --target leveldb && cmake --install build/leveldb
# cgo: -I$PWD/prefix/include, -L$PWD/prefix/lib -lleveldb -lsnappy -lstdc++ -lpthread -lm
```

The benchmark is one Go program for all three stores, with the same procedure and the settings of section 5. For LevelDB it uses a thin cgo wrapper of `leveldb/c.h`. Run the tests on the device the store will use: on tmpfs `fsync` is free and the results differ.
