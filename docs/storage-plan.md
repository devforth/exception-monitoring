# Storage plan: from SQLite to goleveldb

Working plan for the `storage-goleveldb` branch. It replaces SQLite with [goleveldb](https://github.com/syndtr/goleveldb), the Go implementation of LevelDB, and puts a buffer in front of ingest. Update it as decisions are made, and delete it once the work is merged and `CLAUDE.md` describes the result.

The branch `archive/pebble` holds an earlier attempt on Pebble that went as far as the cleanup (stage 4 here). Its pipeline, key layout, counters and cleanup do not depend on the engine and serve as the reference implementation. Only the calls into the store change.

## Goal

Accept every error when many applications report at once. Target: **50 DSNs, each sending 1000 errors at the same moment.** The default rate limit is 1000 envelopes per minute per project.

- A burst of 50 000 events is accepted without a 429, with half the answers within a few milliseconds and none over ~300 ms (SDKs wait seconds).
- The burst is stored within a minute, so sustained processing is at least ~830 events/s.
- Logs: at least 10 000 lines/s sustained.
- Memory stays around 200 MB at the peak of such a burst (a default `GOMEMLIMIT` of 200 MiB; 150 MB would cost speed), and the image under the 50 MB budget.

Baseline today (SQLite, one transaction per request): about **700 events/s** and **1100 log lines/s**. The single SQLite writer is the limit.

## Decisions

**goleveldb.** The comparison of Pebble, LevelDB (C++) and goleveldb is in `docs/storage-audit.md`. The deciding points:

- **Pure Go.** The binary stays static, the image `FROM scratch`, and the arm64 build unchanged. The C++ LevelDB would need cgo, a C++ toolchain per architecture and static libstdc++.
- **Same on-disk format as LevelDB C++, verified both ways.** A database written by one opens and reads correctly in the other, with or without bloom filters. If goleveldb ever needs a fix it does not get, the C++ LevelDB is a way out without migrating data.
- **Small code** (~18k lines), which can be forked if needed.
- **No longer maintained** (last commit 2022). That risk is accepted because of the format compatibility above.

**Keep the format compatible with LevelDB C++.**

- Snappy compression only: zstd exists only on the C++ `main` branch, and goleveldb cannot read it.
- The default byte-wise comparator.

**No SQLite, no hybrid.** Everything moves to goleveldb, the small relational data too (users, sessions, projects, notifications, OAuth), and the SQLite driver leaves the module. Nothing is released yet, so there is no importer of existing databases.

**Search in logs comes later.** The first version reads logs newest first within the filters, with no text index, so writing stays cheap.

## Database on disk

A goleveldb database is a flat directory managed by the library. It is not one file.

| Files | What they hold |
|---|---|
| `*.ldb` | Sorted tables (SSTables), Snappy-compressed |
| `*.log` | The write-ahead journal: recent writes not yet in tables |
| `MANIFEST-*` | Which table is on which level |
| `CURRENT` | The name of the current manifest |
| `LOCK` | Keeps a second process out |
| `LOG` | The library's own text log (compactions) |

- **Location:** `DATA_DIR/leveldb`. Each store keeps its own directory (see *Engine*). `DATA_DIR/database.db` (SQLite) stays until stage 5, and `secret.key` stays as it is.
- **Settings, to be confirmed by measurements in stage 3:**
  - block cache 32 MB;
  - write buffer (memtable) 16 MB;
  - bloom filter of 10 bits per key: every new event looks up its `event_id`, which is almost always missing, and that is the case filters help with;
  - table files of 8 MB instead of the default 2 MB, so that 2 GB is about 250 files rather than 1000, under the open-files cache.
- **Database size:** the sum of the file sizes in the directory of the engine built in, a few hundred files; nothing else counts.
- **Backup:** never a copy of the directory of a running database. A snapshot is copied into a new directory instead (stage 6).

## Ingest pipeline

```
SDK -> HTTP handler -> inbox (goleveldb) -> 200
                         |
          processors (one per core): parse, scrub, compact, group
                         |
          writer (one): one batch every few ms: events, issues, logs,
          indexes, counters, and the deletion of the inbox entries
```

1. **Handler.** The cheap checks stay synchronous: CORS, DSN key, project, rate limit, body size, decompression. Then the raw envelope is appended to the inbox with the project and the receive time, and the SDK gets 200.
2. **Processors.** A pool sized to `GOMAXPROCS`, fed straight from the handler through a channel. The inbox is reread at start, after a full channel and every few seconds, for entries that did not get through the channel.
3. **Writer.** One goroutine commits the results in one batch, together with the deletion of their inbox entries. Notifications are sent after the commit.
4. **Crash safety.** An entry leaves the inbox in the batch that stores its data, so nothing is lost or stored twice. `event_id` keys keep SDK retries idempotent.
5. **Backpressure.** Above an inbox size limit the handler answers 429 with `Retry-After`.
6. **What changes for SDKs.** A malformed envelope is accepted (200) and dropped while processing, logged at debug level.

## Collections: the layer over goleveldb

goleveldb only knows bytes: no tables, no indexes, no query planner. A thin layer of our own, `internal/storage/kv`, gives records a model-like shape, so that no other code builds a key by hand. It works like a model in an ORM, with indexes declared up front.

**Rule: every model is declared once.** Its key, its indexes and the encoding of its value live in one declaration, and every read, write and delete goes through it. The SQLite layer did not do this: a project's columns were listed separately in the column list, the scan function, `INSERT`, `UPDATE` and the migration, so one new field meant edits in five places. Here a new field or index is one edit in `internal/storage/database/schema.go`, plus a migration when stored records have to change.

```go
// Declared once, in one file (database/schema.go): the primary key and every index.
var Logs = kv.Collection[LogRecord]{
	Prefix: "log",
	Key:    func(l LogRecord) kv.Key { return kv.K(l.LevelRank(), l.Seq) },
	Indexes: kv.Indexes[LogRecord]{
		"project": func(l LogRecord) kv.Key { return kv.K(l.ProjectID, l.LevelRank(), l.Seq) },
		"trace":   func(l LogRecord) kv.Key { return kv.KIf(l.TraceID != "", l.TraceID, l.Seq) },
		"message": func(l LogRecord) kv.Key { return kv.K(l.ProjectID, l.Fingerprint, l.Seq) },
	},
}

batch.Save(Logs, record)   // the record and all its index keys, in one batch
batch.Delete(Logs, record) // the record and all its index keys
logs, err := Logs.By("project", 2, kv.Level("error")).Newest().Limit(20).All(db)
```

- **One place for every key.** All collections, their prefixes and indexes are declared in `internal/storage/database/schema.go`, which is the table under *Data layout* in code. Key parts are encoded by one set of functions:
  - numbers are big-endian, so they sort;
  - text ends with a zero byte, so a prefix never matches a longer value;
  - levels are written as their rank.
  - Prefixes are checked to be unique when the program starts.
- **Indexes kept by the layer.** `Save` and `Delete` always write and remove every key of a record, the way SQL keeps its indexes. A stale index key (a pointer to a deleted record) cannot be produced by hand.
  - Unique indexes (a username, an event ID per project) are declared as such: `Save` refuses a taken value.
  - Optional indexes (a log without a trace) are skipped with `KIf`.
- **Queries only through declared indexes.** A query names an index and the leading parts of its key, then the direction, the bounds and the limit. Extra conditions are a `Filter` over that walk, so a check per row is visible in the code. There is no free `where`: without a planner, it would quietly read the whole store.
- **No reflection.** The layer uses generics and explicit key functions, because ingest writes over 10 000 records/s. Values are encoded by each collection, JSON at first.
- **Other building blocks of the layer:**
  - a single writer for batches;
  - sequence numbers;
  - time marks;
  - byte counters per kind, level and day, updated by `Save` and `Delete`;
  - `meta/schema` with migration functions that rebuild an index or rewrite records when the layout changes.
- **A consistency check.** One function walks a collection and verifies that every index key points to an existing record and that every record has all its index keys. Tests run it for every collection.
- **Not provided:** joins, which are done in Go through index values, and aggregates, which come from counters or from the in-memory issue table.

### Engine: the store under the collections

The collections do not call goleveldb directly. They call a small interface that any ordered key-value store can implement, the way AdminForth keeps the shared logic in one base connector and each database in a small connector.

```
services -> store.Store -> kv: collections, indexes, queries, cleanup   (written once)
                              -> Engine: Get, Batch (Put, Delete), Iterator over a range,
                                         CompactRange, Size, Close      (one adapter per store)
                                   -> goleveldb (the only adapter for now)
```

- **One adapter is written: goleveldb.** Others are written only when a reason appears, never ahead. If one is needed, it is one package of a few hundred lines implementing `Engine`, and nothing above it changes.
- **Ordered key-value stores only.** Candidates are LevelDB C++, Pebble or Badger. SQL databases are not supported.
- **The store is chosen when building, with a build tag.** goleveldb is built by default. Another adapter is compiled only with its tag (`go build -tags <store>`), so the default binary and image carry no other store or driver.
- **Changing the store never moves data, automatically or otherwise.** A server built for another store starts with an empty database in its own directory (`DATA_DIR/<store>`). The old directory is left untouched.
- **Tests of the collections layer** run on goleveldb's in-memory storage, with no disk.

## Data layout

Keys are byte strings in sorted order: numbers are big-endian so they sort, and levels are written as their rank, lowest first. A record is stored once under its primary key. Indexes are extra keys whose value is the primary key, written in the same batch. Every row of this table is a collection in `internal/storage/database/schema.go`; nothing else builds keys.

| Data | Primary key | Indexes |
|---|---|---|
| Inbox | `inbox/<seq>` | |
| Issues | `issue/<id>` | The whole issue table is also kept in memory: list filters, sorts and counts run in Go. |
| Status history | `status/<issue>/<seq>` | |
| Events | `event/<issue>/<seq>` | `event_id/<project>/<event_id>`, `event_tr/<trace>/<seq>`, `event_sp/<span>/<seq>`, `event_age/<level>/<seq>` |
| Logs | `log/<level>/<seq>` | `log_pr/<project>/<level>/<seq>`, `log_tr/<trace>/<seq>`, `log_sp/<span>/<seq>`, `log_msg/<project>/<fingerprint>/<seq>` |
| Time marks | `mark/<10-minute interval>` -> the first sequence number of that interval | |
| Byte counters | `size/<kind><level><day>` | |
| Small data | `user/<id>`, `session/<hash>`, `project/<id>`, `connection/<id>`, `destination/<id>`, `outbox/<id>`, `oauth_*/...` | Unique keys (`user_name/<lowercase>`, ...) checked and written by the writer |
| Metadata | `meta/seq`, `meta/schema` | |

- **Sequence numbers** grow with every record, so `<seq>` orders records by arrival.
- **Time marks** turn a date into a sequence number: a dated list starts and stops by key instead of reading everything, and the cleanup cuts whole days.
- **Values** are JSON, which is readable, easy to evolve and compressed with the blocks.

## Queries

- **Issues page:** the in-memory table. "Any event in range" uses day counts kept with each issue.
- **Issue page:** `event/<issue>/...` newest first for the latest 20. Related logs come through `log_tr` / `log_sp`.
- **Logs page:** one index chosen by the filters (project, level, trace), read newest first. Other filters are checked per row, and the page stops at its limit. Totals are counted up to 10 000, as today.
- **REST API and MCP:** unchanged, through the same services.

## Cleanup by size

Retention by age goes away, with its settings:
- the `RETENTION_*` variables;
- the per-project retention in the UI and in MCP;
- the cleaner that deletes old data every 10 minutes.

One limit stays: `MAX_DATABASE_SIZE` (default 2 GB).

- **When.** At startup, every 10 minutes, and after every ~50 MB written. Cleanup starts when the database reaches 100% of the limit and stops at 80%. Only the goleveldb directory counts.
- **How it decides.** The writer keeps byte counters per kind (log, event, issue), level and day. They drop as soon as data is deleted, so after each step the cleanup knows exactly what it freed. The disk size only calibrates them for compression.
- **Order.** The first three steps always run in full. Each later step runs only while the database is above the goal.
  1. Logs older than a month.
  2. Events older than a month beyond the newest 20 of their issue.
  3. Thinning of the events older than a month (see below).
  4. Logs by level, lowest first (trace and debug before errors), oldest day first.
  5. Events older than a month down to one per issue: none when the issue has recent events.
  6. Every issue down to its newest 20 events, whatever their age.
  7. Events except the latest of each issue, by level, oldest day first.
  8. The latest events: the issue stays with its title, counts and dates, and its page says the event was deleted to save space.
  9. Whole issues, lowest level and longest unseen first.
- **Thinning** keeps the exception, `stacktrace` and `threads` without local variables, plus the message, level, time, release and environment. It drops the rest (breadcrumbs, request, contexts, tags). A thinned event is marked: its page says details were removed, and the cleanup does not thin it again. Progress is kept per level, so a new run continues where the last stopped.
- **Deleting.** LevelDB has no range delete. Records are read in batches of 2000, and each one goes with its index keys, with a short rest between batches. The work grows with what is deleted, not with what is stored.
- **Returning space.** After deleting, the cleanup compacts the key ranges it touched (`CompactRange`), so the next size check sees the smaller size and does not delete again. The cost of this with goleveldb's single compaction thread, under ingest, is measured in stage 4.
- **Caps on repeats.** Copies of one log message are capped at 10 as they are written, a fixed limit and not a setting. Events of an issue are not capped when written: the cleanup trims them as above.

## Stages

| Stage | Content | Done when |
|---|---|---|
| 0. Plan and audit | This plan; `docs/storage-audit.md` | The plan is agreed |
| 1. Interfaces | `store` keeps the records, their rules and the `Store` interface; the SQL is in `sqlite`. **Done** (taken from `archive/pebble`) | Services compile against interfaces |
| 2. Ingest and reads | First the collections layer (see *Collections*): the `Engine` interface with the goleveldb adapter, key encoding, collections with indexes, batches, queries, the consistency check, with its own tests. Then, on top of it: inbox, issues, events, logs, time marks, counters; the pipeline; `storetest.Open` runs every test on either store (`STORE=kv`); the rest passes to SQLite until stage 5 **Done:** the layer is `internal/storage/kv` (engine, keys, collections, batches with counters and sequence numbers, queries and cursors, the consistency check); the store is `internal/storage/database` on it (`schema.go` declares every collection; the rest of `store.Store` passes to SQLite); the ingest pipeline and the server run on it (`DATA_DIR/leveldb`). First measurements (tmpfs, 50 projects): 50 000 events stored at 2 500/s, logs at ~6 400 lines/s; answers p50 4 ms, max 690 ms; peak memory 300–540 MB, so stage 3 has work to do. | The layer's tests pass; UI and API tests pass on goleveldb |
| 3. Measurements | Ingest load test (50 × 1000 events, logs), peak memory, latency of answers; tune the settings above; decide the inbox sync policy **Done** (NVMe, 50 projects at once; `INGEST_DIR`, `INGEST_REPEATS`, `INGEST_ONLY` of the load test): one copy of each batch instead of two; issues written without reading them back; repeated log messages counted by keys from their oldest live copy, copies a batch would delete never written; batches bounded by 4 MB as well; goleveldb's level-0 limits relaxed (write stalls 5.5 s to 0.7 s); the inbox without its own sync. Events 3,500/s, answers p50 2 ms, p99 ~105 ms, max ~140–180 ms; logs ~12,400 lines/s without repeats, ~6,500 when every line repeats; peak memory 230–320 MB, ~200 MB with `GOMEMLIMIT=200MiB` at a 5–8% cost. Memory: a default `GOMEMLIMIT` of 200 MiB (in main, overridden by the variable); the goals above follow these numbers. | The goal is met, or the gap is understood |
| 4. Cleanup | The cleanup above under ingest on a 2 GB database; the retention settings, UI and docs removed. A test runs a burst of writes through the pipeline while the cleanup deletes and compacts: nothing accepted is lost or stored twice, the counters match the stored records, the consistency check passes, and the answers to SDKs stay within the goal | Cleanup tests pass, the burst-with-cleanup test among them; the cleanup does not stall ingest |
| 5. Small data | Users, sessions, projects, OAuth, notifications, outbox, as collections of the same layer | All tests pass; `modernc.org/sqlite` removed from `go.mod` |
| 6. Operations | `backup` through a snapshot, docs, image size, `CLAUDE.md` | Ready for the release |

## Risks

- **Memory.** In the audit goleveldb peaked at 190–230 MB RSS, mostly Go heap, against a 150 MB goal. Stage 3 measures it under our load. Levers: block cache, write buffer, open-files cache, `GOMEMLIMIT`.
- **One compaction thread.** Under a burst, together with the cleanup's compaction, goleveldb may slow writes down once level 0 holds too many files (defaults: slow down at 8, pause at 12). Stage 4 measures it.
- **Inbox sync (decided in stage 3).** A synced inbox write waits for the disk (~13 ms on an encrypted NVMe), and every answer to an SDK waited for one: events were accepted at 2,300/s with answers at 22 ms, logs at ~5,900 lines/s. The inbox now writes without its own sync: the writer's next batch, a few milliseconds later, is synced and takes the earlier writes of the shared journal to the disk with it. A crash of the process loses nothing; a power cut or a kernel crash may lose the envelopes accepted in those last milliseconds. With it, on the same disk: events 3,550/s with answers at 2 ms, logs ~11,700 lines/s.
- **No maintenance upstream.** Mitigated by the format compatibility with LevelDB C++ and the small code.

## Open questions

- The inbox size limit and its default (1 GB in the earlier attempt).
- How long a request may wait for the inbox when the writer lags, before answering 429.
- The search design (stage 2 keeps the simple version).
- Are the logs of a stored error's request kept as long as the error, or cleaned with the other logs?
- Table file size (8 MB proposed) and `GOMEMLIMIT`.

## Structure (done after stage 3)

- `store`: the API of the storage, the one list of every operation (`store.Store`) with the records and their rules.
- `database`: the one implementation of that API (`schema.go` declares every collection); `engine_leveldb.go` is the one file that names the engine.
- `kv`: the layer under it (keys, collections, batches, queries, the `Engine` interface); `kv/leveldb`: the goleveldb adapter; `kv/enginetest`: the test every adapter must pass.
- Adapters could later split into two families, key-value and SQL, sharing their code; not now, while no second database is planned.

## Later, outside this plan

- `ALLOWED_ORIGINS` for real deployments (default `*`).
