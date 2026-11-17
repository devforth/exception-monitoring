package sqlite

import (
	"context"
	"strings"
	"unicode/utf8"
)

// Text search of logs goes through logs_search (migration 22), a trigram
// index: a search of 3 or more characters reads only the logs that hold all
// its 3-character pieces, newest first, and stops after the page; the text of
// each is then checked with LIKE, the same test as without the index. Shorter
// text, and the time until the logs from before the index are added, fall
// back to reading logs from the newest (body LIKE).
//
// Deleted logs stay in the index (RefreshLogSearch fills it anew when they
// outnumber the live ones): the join to logs drops them, and the LIKE check
// keeps a reused id from matching the old text.

// minSearchLength is the shortest text the trigram index can find.
const minSearchLength = 3

// maxSearchTrigrams bounds the pieces a query asks the index for; the LIKE
// check does the rest, so a long text needs only some of them.
const maxSearchTrigrams = 32

// usesSearchIndex reports whether a search for text can use the index.
func (store *Store) usesSearchIndex(ctx context.Context, text string) bool {
	return utf8.RuneCountInString(text) >= minSearchLength && store.LogSearchReady(ctx)
}

// searchMatch is the index query for text: every 3-character piece, each
// quoted so it is one trigram ("tim" AND "ime" AND ...). A piece alone needs
// neither positions nor the text, which the index does not keep.
func searchMatch(text string) string {
	characters := []rune(text)

	var (
		pieces []string
		seen   = map[string]bool{}
	)

	for start := 0; start+minSearchLength <= len(characters) && len(pieces) < maxSearchTrigrams; start++ {
		piece := strings.ToLower(string(characters[start : start+minSearchLength]))

		if !seen[piece] {
			seen[piece] = true
			pieces = append(pieces, `"`+strings.ReplaceAll(piece, `"`, `""`)+`"`)
		}
	}

	return strings.Join(pieces, " AND ")
}

// LogSearchReady reports whether every log is in the search index.
func (store *Store) LogSearchReady(ctx context.Context) bool {
	if store.logSearchReady.Load() {
		return true
	}

	var done, last int64

	if err := store.DB.QueryRowContext(ctx, `SELECT done_id, last_id FROM logs_search_backfill`).Scan(&done, &last); err != nil {
		return false
	}

	if done >= last {
		store.logSearchReady.Store(true)
	}

	return done >= last
}

// IndexLogsForSearch adds up to batch of the logs from before the search
// index to it, oldest first, and reports whether all of them are in. Each
// batch is one short transaction, so ingest waits only for that long.
func (store *Store) IndexLogsForSearch(ctx context.Context, batch int) (bool, error) {
	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return false, err
	}

	defer transaction.Rollback()

	var done, last int64

	if err := transaction.QueryRowContext(ctx, `SELECT done_id, last_id FROM logs_search_backfill`).Scan(&done, &last); err != nil {
		return false, err
	}

	if done >= last {
		store.logSearchReady.Store(true)

		return true, nil
	}

	// The last id of this batch; the end when no logs are left (deleted).
	var upTo int64

	if err := transaction.QueryRowContext(ctx,
		`SELECT COALESCE(MAX(id), ?) FROM (SELECT id FROM logs WHERE id > ? AND id <= ? ORDER BY id LIMIT ?)`,
		last, done, last, batch).Scan(&upTo); err != nil {
		return false, err
	}

	if _, err := transaction.ExecContext(ctx,
		`INSERT INTO logs_search (rowid, body) SELECT id, body FROM logs WHERE id > ? AND id <= ?`, done, upTo); err != nil {
		return false, err
	}

	if _, err := transaction.ExecContext(ctx, `UPDATE logs_search_backfill SET done_id = ?`, upTo); err != nil {
		return false, err
	}

	if err := transaction.Commit(); err != nil {
		return false, err
	}

	return upTo >= last, nil
}

// LogsToIndex is how many logs are not in the search index yet.
func (store *Store) LogsToIndex(ctx context.Context) (int, error) {
	var count int

	err := store.DB.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM logs, logs_search_backfill WHERE id > done_id AND id <= last_id`).Scan(&count)

	return count, err
}

// RefreshLogSearch starts filling the search index anew when it holds more
// deleted logs than live ones (stale, see migration 22): it empties the index
// in one short step and the background indexing (IndexLogsForSearch) adds the
// logs again; until it is done, searches read logs. Reports whether it did.
func (store *Store) RefreshLogSearch(ctx context.Context) (bool, error) {
	var stale, live int64

	if err := store.DB.QueryRowContext(ctx, `SELECT stale, (SELECT COUNT(*) FROM logs) FROM logs_search_backfill`).Scan(&stale, &live); err != nil {
		return false, err
	}

	if stale < minStaleLogs || stale <= live {
		return false, nil
	}

	transaction, err := store.DB.BeginTx(ctx, nil)

	if err != nil {
		return false, err
	}

	defer transaction.Rollback()

	for _, statement := range []string{
		`INSERT INTO logs_search (logs_search) VALUES ('delete-all')`,
		`UPDATE logs_search_backfill SET last_id = (SELECT COALESCE(MAX(id), 0) FROM logs), done_id = 0, stale = 0`,
	} {
		if _, err := transaction.ExecContext(ctx, statement); err != nil {
			return false, err
		}
	}

	if err := transaction.Commit(); err != nil {
		return false, err
	}

	store.logSearchReady.Store(false)

	return true, nil
}

// minStaleLogs keeps a small database from refilling its index for nothing
// (a variable for tests).
var minStaleLogs int64 = 10000
