package database

import (
	"cmp"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/text"
	"github.com/devforth/exception-monitoring/internal/storage/kv"
	model "github.com/devforth/exception-monitoring/internal/storage/store"
)

// issueRecord is an issue as stored (schema.go). The whole table is also
// kept in memory (tens of thousands of rows), so lists, filters, sorts and
// counts run in Go; a change is written in the batch that makes it and put
// in memory once that batch is committed.
type issueRecord struct {
	model.Issue
	Fingerprint string `json:"fingerprint"`
	// Stored is how many of its events are kept on disk (EventCount counts
	// every event ever received); trims read it.
	Stored int `json:"stored"`
	// EventDays counts the stored events per day they were received, oldest
	// day first, for "any event in the date range". One entry per day, not
	// per event, so an issue with a million events stays small.
	EventDays []dayCount `json:"event_days"`

	// storedSize is the size of the stored value, so a change is written
	// without reading the old version back (kv.Collection.Write).
	storedSize int
}

// dayCount is how many stored events of an issue arrived on a day.
type dayCount struct {
	Day   int32 `json:"d"` // days since 1970-01-01, UTC
	Count int   `json:"n"`
}

// addEvent counts a stored event received at.
func (issue *issueRecord) addEvent(at time.Time) {
	issue.Stored++
	number := dayOf(at)
	index, found := slices.BinarySearchFunc(issue.EventDays, number, func(entry dayCount, target int32) int { return cmp.Compare(entry.Day, target) })

	if found {
		issue.EventDays[index].Count++

		return
	}

	issue.EventDays = slices.Insert(issue.EventDays, index, dayCount{Day: number, Count: 1})
}

// removeEvent forgets a stored event received at.
func (issue *issueRecord) removeEvent(at time.Time) {
	issue.Stored = max(issue.Stored-1, 0)
	number := dayOf(at)
	index, found := slices.BinarySearchFunc(issue.EventDays, number, func(entry dayCount, target int32) int { return cmp.Compare(entry.Day, target) })

	if !found {
		return
	}

	if issue.EventDays[index].Count--; issue.EventDays[index].Count <= 0 {
		issue.EventDays = slices.Delete(issue.EventDays, index, index+1)
	}
}

// copy returns an issue the caller may change without touching this one.
func (issue *issueRecord) copy() *issueRecord {
	duplicate := *issue
	duplicate.EventDays = slices.Clone(issue.EventDays)

	if issue.ResolvedAt != nil {
		resolvedAt := *issue.ResolvedAt
		duplicate.ResolvedAt = &resolvedAt
	}

	return &duplicate
}

type issueTable struct {
	byID          map[int64]*issueRecord
	byFingerprint map[string]*issueRecord
	lastID        int64
}

// fingerprintKey names an issue in memory by its project and fingerprint.
func fingerprintKey(projectID int, fingerprint string) string {
	return string(kv.K(kv.U32(uint32(projectID)), kv.Text(fingerprint)))
}

func (table *issueTable) put(issue *issueRecord) {
	table.byID[issue.ID] = issue
	table.byFingerprint[fingerprintKey(issue.ProjectID, issue.Fingerprint)] = issue
	table.lastID = max(table.lastID, issue.ID)
}

func (table *issueTable) remove(id int64) {
	if issue, ok := table.byID[id]; ok {
		delete(table.byID, id)
		delete(table.byFingerprint, fingerprintKey(issue.ProjectID, issue.Fingerprint))
	}
}

func (database *Store) loadIssues() error {
	table := &issueTable{byID: map[int64]*issueRecord{}, byFingerprint: map[string]*issueRecord{}}

	err := database.issues.Primary().Each(database.db, func(entry kv.Entry[issueRecord]) (bool, error) {
		issue := entry.Record
		issue.storedSize = entry.Size
		table.put(&issue)

		return true, nil
	})

	database.table = table

	return err
}

// match is what the issue table says of an issue and a filter.
type match int

const (
	noMatch match = iota
	isMatch
	// checkEvents: only events on a day the range covers in part, so the
	// times of its stored events decide (the UI's ranges are whole days).
	checkEvents
)

// inRange reports whether at is within the filter's dates.
func inRange(filter model.IssueFilter, at time.Time) bool {
	return (filter.SeenSince.IsZero() || !at.Before(filter.SeenSince)) && (filter.SeenUntil.IsZero() || at.Before(filter.SeenUntil))
}

// matches tells whether an issue passes the filter.
func matches(issue *issueRecord, filter model.IssueFilter) match {
	if len(filter.ProjectIDs) > 0 && !slices.Contains(filter.ProjectIDs, issue.ProjectID) ||
		filter.Status != "" && issue.Status != filter.Status ||
		len(filter.Levels) > 0 && !slices.Contains(filter.Levels, issue.Level) ||
		filter.Query != "" && !text.ContainsFold(issue.Title, filter.Query) && !text.ContainsFold(issue.Culprit, filter.Query) {
		return noMatch
	}

	if filter.SeenSince.IsZero() && filter.SeenUntil.IsZero() {
		return isMatch
	}

	yes := func(ok bool) match {
		if ok {
			return isMatch
		}

		return noMatch
	}

	switch filter.DateField {
	case model.DateFirstSeen:
		return yes(inRange(filter, issue.FirstSeen))
	case model.DateLastSeen:
		return yes(inRange(filter, issue.LastSeen))
	}

	result := noMatch

	for _, entry := range issue.EventDays {
		start := dayStart(entry.Day)

		switch whole, touched := inRange(filter, start) && inRange(filter, start.Add(day-time.Nanosecond)),
			(filter.SeenUntil.IsZero() || start.Before(filter.SeenUntil)) && (filter.SeenSince.IsZero() || start.Add(day).After(filter.SeenSince)); {
		case whole:
			return isMatch
		case touched:
			result = checkEvents
		}
	}

	return result
}

// anyEventInRange reads the stored events of an issue for checkEvents.
func (database *Store) anyEventInRange(issueID int64, filter model.IssueFilter) (bool, error) {
	found := false

	err := database.events.Primary(kv.U64(uint64(issueID))).Newest().Each(database.db, func(entry kv.Entry[eventRecord]) (bool, error) {
		found = inRange(filter, entry.Record.ReceivedAt)

		return !found, nil
	})

	return found, err
}

// filterIssues returns copies of the issues passing the filter, unsorted.
// Issues that need their events checked are checked after the table is
// released, so a read from disk never holds up the writer.
func (database *Store) filterIssues(filter model.IssueFilter) ([]model.Issue, error) {
	database.issuesMutex.RLock()

	var issues, check []model.Issue

	for _, issue := range database.table.byID {
		switch matches(issue, filter) {
		case isMatch:
			issues = append(issues, issue.Issue)
		case checkEvents:
			check = append(check, issue.Issue)
		case noMatch:
		}
	}

	database.issuesMutex.RUnlock()

	for _, issue := range check {
		found, err := database.anyEventInRange(issue.ID, filter)

		if err != nil {
			return nil, err
		}

		if found {
			issues = append(issues, issue)
		}
	}

	return issues, nil
}

// selectIssues returns copies of the issues passing the filter, sorted.
func (database *Store) selectIssues(ctx context.Context, filter model.IssueFilter) ([]model.Issue, error) {
	names := map[int]string{}

	if filter.Sort == "project" || filter.Sort == "project_desc" {
		projects, err := database.ListProjects(ctx)

		if err != nil {
			return nil, err
		}

		for _, project := range projects {
			names[project.ID] = strings.ToLower(project.Name)
		}
	}

	issues, err := database.filterIssues(filter)

	if err != nil {
		return nil, err
	}

	slices.SortFunc(issues, issueOrder(filter.Sort, names))

	return issues, nil
}

// issueOrder compares issues for a sort of model.IssueSorts; the id breaks
// ties so pages stay stable.
func issueOrder(sort string, projectNames map[int]string) func(a, b model.Issue) int {
	newestFirst := func(a, b model.Issue) int {
		return cmp.Or(b.LastSeen.Compare(a.LastSeen), cmp.Compare(b.ID, a.ID))
	}

	switch sort {
	case "id":
		return func(a, b model.Issue) int { return cmp.Compare(b.ID, a.ID) }
	case "id_asc":
		return func(a, b model.Issue) int { return cmp.Compare(a.ID, b.ID) }
	case "project":
		return func(a, b model.Issue) int {
			return cmp.Or(cmp.Compare(projectNames[a.ProjectID], projectNames[b.ProjectID]), newestFirst(a, b))
		}
	case "project_desc":
		return func(a, b model.Issue) int {
			return cmp.Or(cmp.Compare(projectNames[b.ProjectID], projectNames[a.ProjectID]), newestFirst(a, b))
		}
	case "last_seen_asc":
		return func(a, b model.Issue) int { return cmp.Or(a.LastSeen.Compare(b.LastSeen), cmp.Compare(a.ID, b.ID)) }
	case "first_seen":
		return func(a, b model.Issue) int { return cmp.Or(b.FirstSeen.Compare(a.FirstSeen), cmp.Compare(b.ID, a.ID)) }
	case "first_seen_asc":
		return func(a, b model.Issue) int { return cmp.Or(a.FirstSeen.Compare(b.FirstSeen), cmp.Compare(a.ID, b.ID)) }
	case "event_count":
		return func(a, b model.Issue) int {
			return cmp.Or(cmp.Compare(b.EventCount, a.EventCount), cmp.Compare(b.ID, a.ID))
		}
	case "event_count_asc":
		return func(a, b model.Issue) int {
			return cmp.Or(cmp.Compare(a.EventCount, b.EventCount), cmp.Compare(a.ID, b.ID))
		}
	default: // last_seen
		return newestFirst
	}
}

// CountIssues counts the issues matching the filter (Limit and Offset are ignored).
func (database *Store) CountIssues(_ context.Context, filter model.IssueFilter) (int, error) {
	issues, err := database.filterIssues(filter)

	return len(issues), err
}

// ListIssues returns a page of the issues matching the filter.
func (database *Store) ListIssues(ctx context.Context, filter model.IssueFilter) ([]model.Issue, error) {
	issues, err := database.selectIssues(ctx, filter)

	if err != nil {
		return nil, err
	}

	start := min(max(filter.Offset, 0), len(issues))
	end := len(issues)

	if filter.Limit > 0 {
		end = min(start+filter.Limit, len(issues))
	}

	return issues[start:end], nil
}

func (database *Store) GetIssue(_ context.Context, issueID int64) (model.Issue, error) {
	database.issuesMutex.RLock()

	defer database.issuesMutex.RUnlock()

	issue, ok := database.table.byID[issueID]

	if !ok {
		return model.Issue{}, model.ErrNotFound
	}

	return issue.Issue, nil
}

// GetIssues returns the issues found among the ids, in the order given.
func (database *Store) GetIssues(_ context.Context, issueIDs []int64) ([]model.Issue, error) {
	database.issuesMutex.RLock()

	defer database.issuesMutex.RUnlock()

	issues := []model.Issue{}

	for _, id := range issueIDs {
		if issue, ok := database.table.byID[id]; ok {
			issues = append(issues, issue.Issue)
		}
	}

	return issues, nil
}

// UnresolvedCounts returns the number of unresolved issues per project.
func (database *Store) UnresolvedCounts(context.Context) (map[int]int, error) {
	database.issuesMutex.RLock()

	defer database.issuesMutex.RUnlock()

	counts := map[int]int{}

	for _, issue := range database.table.byID {
		if issue.Status == model.StatusUnresolved {
			counts[issue.ProjectID]++
		}
	}

	return counts, nil
}
