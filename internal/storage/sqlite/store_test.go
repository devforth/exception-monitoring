package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestOpenCreatesDatabaseInWALMode(t *testing.T) {
	ctx := context.Background()
	database, err := Open(ctx, filepath.Join(t.TempDir(), "nested", "test.db"))

	if err != nil {
		t.Fatal(err)
	}

	defer closeDatabase(t, database)

	var journalMode string

	if err := database.DB.QueryRowContext(ctx, `PRAGMA journal_mode`).Scan(&journalMode); err != nil {
		t.Fatal(err)
	}

	if journalMode != "wal" {
		t.Fatalf("journal_mode = %q, want wal", journalMode)
	}

	if err := database.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMigrationsApplyOnceAndInOrder(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	migrationList := []migration{
		{1, `CREATE TABLE a (id INTEGER PRIMARY KEY)`},
		{2, `ALTER TABLE a ADD COLUMN name TEXT`},
	}

	database, err := openWithMigrations(ctx, path, migrationList)

	if err != nil {
		t.Fatal(err)
	}

	closeDatabase(t, database)

	// Reopening must not re-apply (ALTER TABLE would fail on a duplicate column).
	migrationList = append(migrationList, migration{3, `CREATE TABLE b (id INTEGER PRIMARY KEY)`})
	database, err = openWithMigrations(ctx, path, migrationList)

	if err != nil {
		t.Fatal(err)
	}

	defer closeDatabase(t, database)

	var version, count int

	if err := database.DB.QueryRowContext(ctx, `SELECT MAX(version), COUNT(*) FROM schema_migrations`).Scan(&version, &count); err != nil {
		t.Fatal(err)
	}

	if version != 3 || count != 3 {
		t.Fatalf("version=%d count=%d, want 3 and 3", version, count)
	}

	if _, err := database.DB.ExecContext(ctx, `INSERT INTO a (id, name) VALUES (1, 'x'); INSERT INTO b (id) VALUES (1)`); err != nil {
		t.Fatal(err)
	}
}

func TestFailedMigrationIsRolledBack(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	migrationList := []migration{
		{1, `CREATE TABLE a (id INTEGER PRIMARY KEY); THIS IS NOT SQL`},
	}

	if _, err := openWithMigrations(ctx, path, migrationList); err == nil {
		t.Fatal("expected error for invalid migration")
	}

	database, err := openWithMigrations(ctx, path, nil)

	if err != nil {
		t.Fatal(err)
	}

	defer closeDatabase(t, database)

	var tableCount int

	if err := database.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE name = 'a'`).Scan(&tableCount); err != nil {
		t.Fatal(err)
	}

	if tableCount != 0 {
		t.Fatal("table from failed migration must not exist")
	}
}

func TestNewerSchemaRejected(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")
	database, err := openWithMigrations(ctx, path, []migration{{1, `SELECT 1`}, {2, `SELECT 1`}})

	if err != nil {
		t.Fatal(err)
	}

	closeDatabase(t, database)

	// Simulates rolling back to an older binary that knows only migration 1.
	_, err = openWithMigrations(ctx, path, []migration{{1, `SELECT 1`}})

	if err == nil || !strings.Contains(err.Error(), "newer than this binary") {
		t.Fatalf("got %v, want 'newer than this binary' error", err)
	}
}

// Read-then-write transactions running at the same time must wait for each other
// instead of failing with "database is locked".
func TestConcurrentReadThenWriteTransactions(t *testing.T) {
	ctx := context.Background()
	database, err := openWithMigrations(ctx, filepath.Join(t.TempDir(), "test.db"), []migration{
		{1, `CREATE TABLE counters (name TEXT PRIMARY KEY, n INTEGER NOT NULL)`},
	})

	if err != nil {
		t.Fatal(err)
	}

	defer closeDatabase(t, database)

	const workers = 20

	var waitGroup sync.WaitGroup

	results := make(chan error, workers)

	for range workers {
		waitGroup.Add(1)

		go func() {
			defer waitGroup.Done()

			results <- func() error {
				transaction, err := database.DB.BeginTx(ctx, nil)

				if err != nil {
					return err
				}

				defer transaction.Rollback()

				var current int

				err = transaction.QueryRowContext(ctx, `SELECT n FROM counters WHERE name = 'x'`).Scan(&current)

				if err != nil && err != sql.ErrNoRows {
					return err
				}

				if _, err := transaction.ExecContext(ctx, `INSERT INTO counters (name, n) VALUES ('x', ?)
					ON CONFLICT(name) DO UPDATE SET n = ?`, current+1, current+1); err != nil {
					return err
				}

				return transaction.Commit()
			}()
		}()
	}

	waitGroup.Wait()
	close(results)

	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}

	var counter int

	if err := database.DB.QueryRowContext(ctx, `SELECT n FROM counters WHERE name = 'x'`).Scan(&counter); err != nil {
		t.Fatal(err)
	}

	if counter != workers {
		t.Fatalf("counter = %d, want %d (lost updates)", counter, workers)
	}
}

func TestNonSequentialMigrationsRejected(t *testing.T) {
	_, err := openWithMigrations(context.Background(), filepath.Join(t.TempDir(), "test.db"), []migration{{2, `SELECT 1`}})

	if err == nil {
		t.Fatal("expected error for non-sequential versions")
	}
}

// closeDatabase closes the store and fails the test if that fails.
func closeDatabase(t *testing.T, database *Store) {
	t.Helper()

	if err := database.Close(); err != nil {
		t.Error(err)
	}
}

// Retention gives deleted space back to the file system only with
// incremental auto_vacuum; a database created without it is switched once.
func TestOpenEnablesIncrementalVacuum(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "test.db")

	plain, err := sql.Open("sqlite", "file:"+path)

	if err != nil {
		t.Fatal(err)
	}

	if _, err := plain.Exec(`CREATE TABLE old_data (value TEXT)`); err != nil {
		t.Fatal(err)
	}

	_ = plain.Close()

	database, err := Open(ctx, path)

	if err != nil {
		t.Fatal(err)
	}

	defer closeDatabase(t, database)

	var mode int

	if err := database.DB.QueryRow(`PRAGMA auto_vacuum`).Scan(&mode); err != nil || mode != 2 {
		t.Fatalf("auto_vacuum = %d (%v), want 2 (incremental)", mode, err)
	}
}
