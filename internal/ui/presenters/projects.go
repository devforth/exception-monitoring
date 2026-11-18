package presenters

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
	"github.com/devforth/exception-monitoring/internal/ui/lists"
)

// Projects presents projects: the table of the Projects page and the form.
type Projects struct {
	Recording config.Recording // the global levels to record
	// FromEnv: the project comes from PROJECTS (only its color and levels change).
	FromEnv func(projectID int) bool
	// Color is the color a project shows (services.Projects.Color).
	Color func(project store.Project) string
}

// Info is a row of the projects table.
func (presenter Projects) Info(project config.Project, dsn string, unresolvedIssues int) contracts.ProjectInfo {
	return contracts.ProjectInfo{
		ID: project.ID, Name: project.Name, DSN: dsn, UnresolvedIssues: unresolvedIssues,
		FromEnv:     presenter.FromEnv(project.ID),
		EventLevels: config.InLevelOrder(presenter.Recording.EventLevelsFor(project.EventLevels), config.Levels),
		LogLevels:   presenter.Recording.LogLevelsFor(project.LogLevels),
	}
}

// Form fills the form from a project (empty for a new one).
func (presenter Projects) Form(project store.Project) contracts.ProjectView {
	return contracts.ProjectView{
		EventLevels: lists.LevelOptions(config.Levels, presenter.Recording.EventLevelsFor(project.EventLevels)),
		LogLevels:   lists.LevelOptions(config.LogLevels, presenter.Recording.LogLevelsFor(project.LogLevels)),
		New:         project.ID == 0, ID: project.ID, Name: project.Name, FromEnv: project.ID != 0 && presenter.FromEnv(project.ID),
		Color:  presenter.Color(project),
		Errors: map[string]string{},
	}
}

// Storage shows how much the database holds (used bytes) and how the
// cleanup keeps it under the limit.
func Storage(used int64, settings config.Retention) contracts.StorageInfo {
	info := contracts.StorageInfo{
		Used: config.FormatSize(used), CleanupStop: config.CleanupStopPercent,
	}

	if settings.MaxDatabaseBytes > 0 {
		info.Limit = config.FormatSize(settings.MaxDatabaseBytes)
		info.Percent = int(min(100, used*100/settings.MaxDatabaseBytes))
	}

	return info
}

// CleanupRuns shows the runs of the cleanup; projectName names a project by id.
func CleanupRuns(runs []store.CleanupRun, projectName func(id int) string) []contracts.CleanupRunView {
	views := make([]contracts.CleanupRunView, 0, len(runs))

	for _, run := range runs {
		stop := run.Limit * config.CleanupStopPercent / 100
		view := contracts.CleanupRunView{
			ID: run.Sequence, StartedAt: run.StartedAt, Status: run.Status, Reached: run.Reached, Error: run.Error,
			Limit: config.FormatSize(run.Limit), SizeBefore: config.FormatSize(run.SizeBefore),
			Compacted: config.FormatSize(run.Compacted), SizeAfter: config.FormatSize(run.SizeAfter),
			Stop: config.FormatSize(stop), Needed: config.FormatSize(max(run.Compacted-stop, 0)),
			Deleted: stepNumbers(run.Steps), Result: cleanupResult(run),
			Duration: tookText(run.Duration),
		}

		for _, step := range run.Steps {
			stepView := contracts.CleanupStepView{
				Number: step.Number, Name: step.Name, Freed: config.FormatSize(step.Freed),
				Duration: tookText(step.Duration),
			}

			for _, id := range slices.Sorted(maps.Keys(step.Projects)) {
				stepView.Projects = append(stepView.Projects, contracts.CleanupProjectView{Project: projectName(id), Deleted: deleted(*step.Projects[id])})
			}

			view.Steps = append(view.Steps, stepView)
		}

		views = append(views, view)
	}

	return views
}

// tookText rounds to milliseconds under 10 s, to seconds above.
func tookText(took time.Duration) string {
	if took < 10*time.Second {
		return took.Round(time.Millisecond).String()
	}

	return took.Round(time.Second).String()
}

// cleanupResult says why a run stopped.
func cleanupResult(run store.CleanupRun) string {
	switch {
	case run.Status == store.CleanupRunning:
		return fmt.Sprintf("running, at step %d", run.Reached)
	case run.Status == store.CleanupFailed:
		return fmt.Sprintf("failed in step %d", run.Reached)
	case run.Reached == 0:
		return "compacting was enough, nothing deleted"
	case run.Reached <= store.CleanupAlwaysSteps:
		return fmt.Sprintf("steps 1–%d always run; after them the database was under 80%%", store.CleanupAlwaysSteps)
	case run.Reached < store.CleanupSteps:
		return fmt.Sprintf("stopped in step %d: the database was at 80%%", run.Reached)
	default:
		return "every step ran"
	}
}

// stepNumbers reads the steps that deleted something as "1, 4–7".
func stepNumbers(steps []store.CleanupStep) string {
	var parts []string

	for index := 0; index < len(steps); {
		end := index

		for end+1 < len(steps) && steps[end+1].Number == steps[end].Number+1 {
			end++
		}

		if end > index {
			parts = append(parts, fmt.Sprintf("%d–%d", steps[index].Number, steps[end].Number))
		} else {
			parts = append(parts, strconv.Itoa(steps[index].Number))
		}

		index = end + 1
	}

	return strings.Join(parts, ", ")
}

// deleted reads "120 logs, 3 events, 5 events thinned".
func deleted(counts store.CleanupCounts) string {
	var parts []string

	for _, part := range []struct {
		count int
		name  string
	}{{counts.Logs, "logs"}, {counts.Events, "events"}, {counts.Thinned, "events thinned"}, {counts.Issues, "issues"}} {
		if part.count > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", part.count, part.name))
		}
	}

	return strings.Join(parts, ", ")
}
