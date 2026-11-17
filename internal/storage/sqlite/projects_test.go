package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"
)

// Projects from PROJECTS are written at every start: created once, then kept
// in line with env for name and key, while retention set in the UI stays.
func TestSeedProject(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()

	backend := Project{ID: 1, Name: "backend", PublicKey: "aaaaaaaaaaaaaaaa"}

	for range 2 { // the same values again change nothing
		if err := database.SeedProject(ctx, backend, baseTime); err != nil {
			t.Fatal(err)
		}
	}

	logs := 72 * time.Hour
	stored, _ := database.GetProject(ctx, 1)
	stored.RetentionLogs = &logs

	if err := database.UpdateProject(ctx, stored, baseTime); err != nil {
		t.Fatal(err)
	}

	// A new key in env (rotated) replaces the stored one; retention stays.
	backend.PublicKey = "bbbbbbbbbbbbbbbb"

	if err := database.SeedProject(ctx, backend, baseTime); err != nil {
		t.Fatal(err)
	}

	stored, _ = database.GetProject(ctx, 1)

	if stored.PublicKey != "bbbbbbbbbbbbbbbb" || stored.RetentionLogs == nil || *stored.RetentionLogs != logs {
		t.Fatalf("after a key change: %+v", stored)
	}

	// Another id with a taken name or key is refused.
	for _, taken := range []Project{
		{ID: 2, Name: "backend", PublicKey: "cccccccccccccccc"},
		{ID: 2, Name: "frontend", PublicKey: "bbbbbbbbbbbbbbbb"},
	} {
		if err := database.SeedProject(ctx, taken, baseTime); !errors.Is(err, ErrProjectConflict) {
			t.Errorf("seed %+v: %v", taken, err)
		}
	}
}

func TestDeleteProjectRemovesItsData(t *testing.T) {
	database := openTestStore(t)
	ctx := context.Background()

	for _, project := range []Project{{ID: 1, Name: "backend", PublicKey: "aaaaaaaaaaaaaaaa"}, {ID: 2, Name: "frontend", PublicKey: "bbbbbbbbbbbbbbbb"}} {
		if err := database.SeedProject(ctx, project, baseTime); err != nil {
			t.Fatal(err)
		}
	}

	for _, projectID := range []int{1, 2} {
		event := testEvent("e"+string(rune('0'+projectID)), "fp", baseTime)
		event.ProjectID = projectID
		save(t, database, event)

		if err := database.SaveLogs(ctx, []NewLog{{ProjectID: projectID, Timestamp: baseTime, Level: "info", Body: "line", Attributes: []byte(`{}`)}}, 0); err != nil {
			t.Fatal(err)
		}
	}

	if err := database.DeleteProject(ctx, 2); err != nil {
		t.Fatal(err)
	}

	var left, kept int

	_ = database.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM issues WHERE project_id = 2) + (SELECT COUNT(*) FROM events WHERE project_id = 2) + (SELECT COUNT(*) FROM logs WHERE project_id = 2)`).Scan(&left)
	_ = database.DB.QueryRow(`SELECT (SELECT COUNT(*) FROM issues WHERE project_id = 1) + (SELECT COUNT(*) FROM events WHERE project_id = 1) + (SELECT COUNT(*) FROM logs WHERE project_id = 1)`).Scan(&kept)

	if left != 0 || kept != 3 {
		t.Fatalf("rows of the deleted project: %d, of the other: %d", left, kept)
	}

	if err := database.DeleteProject(ctx, 2); !errors.Is(err, ErrNotFound) {
		t.Fatalf("delete twice: %v", err)
	}
}
