package store

import (
	"context"
	"time"
)

// WritePause is the rest between the steps of long background work
// (retention, filling the search index): each step is a write transaction,
// and a writer waiting for the lock (ingest) must get it in between. SQLite's
// busy handler retries every 100 ms at most, so a shorter rest goes unseen.
const WritePause = 100 * time.Millisecond

// Pause rests WritePause, or less if ctx ends (its error).
func Pause(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(WritePause):
		return nil
	}
}

// ProjectScope limits a retention delete to some projects: the listed ones,
// or with Others every project that is not listed (data of projects with
// their own retention is handled by their own scope).
type ProjectScope struct {
	IDs    []int
	Others bool
}
