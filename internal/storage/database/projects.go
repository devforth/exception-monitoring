package database

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// deleteChunk is how many logs one batch of a project deletion removes.
const deleteChunk = 5000

// projectIDs: a project added in the UI gets a random 8-digit id (PROJECTS sets its own).
const (
	firstProjectID = 10_000_000
	projectIDRange = 90_000_000
)

// keepNone keeps "no level" (a set but empty list) apart from nil, "the defaults", through JSON.
func keepNone(project model.Project) model.Project {
	for _, levels := range []**[]string{&project.EventLevels, &project.LogLevels} {
		if *levels != nil && **levels == nil {
			*levels = &[]string{}
		}
	}

	return project
}

func (database *Store) ListProjects(context.Context) ([]model.Project, error) {
	return database.projects.Primary().All(database.db)
}

func (database *Store) GetProject(_ context.Context, id int) (model.Project, error) {
	project, err := database.projects.Get(database.db, kv.K(kv.U32(uint32(id))))

	return project, notFound(err)
}

// projectConflict turns a taken name or key into ErrProjectConflict.
func projectConflict(err error) error {
	if conflict := (kv.ErrConflict{}); errors.As(err, &conflict) {
		return fmt.Errorf("%w: %s", model.ErrProjectConflict, conflict.Index)
	}

	return err
}

// CreateProject adds a project with a random 8-digit id that is not taken.
func (database *Store) CreateProject(_ context.Context, project model.Project, now time.Time) (int, error) {
	project.CreatedAt = now.UTC()

	err := database.db.Update(true, func(batch *kv.Batch) error {
		for {
			number, err := rand.Int(rand.Reader, big.NewInt(projectIDRange))

			if err != nil {
				return err
			}

			project.ID = firstProjectID + int(number.Int64())

			if _, err := database.projects.Get(batch, kv.K(kv.U32(uint32(project.ID)))); errors.Is(err, kv.ErrNotFound) {
				break
			} else if err != nil {
				return err
			}
		}

		return projectConflict(database.projects.Insert(batch, keepNone(project)))
	})

	return project.ID, err
}

// UpdateProject saves the settings of a project.
func (database *Store) UpdateProject(_ context.Context, project model.Project) error {
	return database.db.Update(true, func(batch *kv.Batch) error {
		stored, err := database.projects.Get(batch, kv.K(kv.U32(uint32(project.ID))))

		if err != nil {
			return notFound(err)
		}

		project.CreatedAt = stored.CreatedAt

		return projectConflict(database.projects.Save(batch, keepNone(project)))
	})
}

// SeedProject writes a project from PROJECTS: created if missing, otherwise
// its name and key are set to the env values (settings from the UI stay). A
// name or key taken by another project is an error, so a typo in PROJECTS
// cannot take over another DSN.
func (database *Store) SeedProject(_ context.Context, project model.Project, now time.Time) error {
	err := database.db.Update(true, func(batch *kv.Batch) error {
		stored, err := database.projects.Get(batch, kv.K(kv.U32(uint32(project.ID))))

		switch {
		case errors.Is(err, kv.ErrNotFound):
			project.CreatedAt = now.UTC()

			return projectConflict(database.projects.Insert(batch, project))
		case err != nil:
			return err
		case stored.Name == project.Name && stored.PublicKey == project.PublicKey:
			return nil
		}

		stored.Name, stored.PublicKey = project.Name, project.PublicKey

		return projectConflict(database.projects.Save(batch, stored))
	})

	if err != nil {
		return fmt.Errorf("project %d %q: %w", project.ID, project.Name, err)
	}

	return nil
}

// DeleteProject deletes the project and its place in connections first, so its
// DSN stops at once, then its data in batches.
func (database *Store) DeleteProject(ctx context.Context, id int) error {
	err := database.db.Update(true, func(batch *kv.Batch) error {
		project, err := database.projects.Get(batch, kv.K(kv.U32(uint32(id))))

		if err != nil {
			return notFound(err)
		}

		if err := database.projects.Delete(batch, project); err != nil {
			return err
		}

		return database.dropProjectFromConnections(batch, id)
	})

	if err != nil {
		return err
	}

	database.issuesMutex.RLock()

	var issueIDs []string

	for issueID, issue := range database.table.byID {
		if issue.ProjectID == id {
			issueIDs = append(issueIDs, issueID)
		}
	}

	database.issuesMutex.RUnlock()

	if _, err := database.DeleteIssues(ctx, issueIDs); err != nil {
		return err
	}

	// Each chunk goes on after the last log deleted, so no walk passes deleted keys again.
	logs := database.logs.By("project", kv.U32(uint32(id)))

	for ctx.Err() == nil {
		deleted := 0

		err := database.update(func(writer *batchWriter) error {
			return logs.Limit(deleteChunk).Each(database.db, func(entry kv.Entry[logRecord]) (bool, error) {
				database.logs.DeleteEntry(writer.batch, entry)
				logs = database.logs.By("project", kv.U32(uint32(id))).From(kv.U8(levelRank(entry.Record.Level)), kv.U64(entry.Record.Sequence+1))
				deleted++

				return true, nil
			})
		})

		if err != nil {
			return err
		}

		if deleted < deleteChunk {
			return errors.Join(database.db.CompactRange(database.logs.KeyRange()), database.db.CompactRange(database.events.KeyRange()))
		}
	}

	return ctx.Err()
}
