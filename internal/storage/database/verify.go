package database

import (
	"fmt"
	"maps"

	"github.com/devforth/exception-monitoring/internal/storage/kv"
)

// Verify checks index keys, byte counters and issue event counts; it reads everything.
func (database *Store) Verify() ([]string, error) {
	var problems []string

	checks := []func(kv.Reader) ([]string, error){
		database.inbox.Check, database.issues.Check, database.status.Check, database.events.Check,
		database.logs.Check, database.collections.marks.Check, database.thin.Check, database.cleanups.Check,
		database.users.Check, database.sessions.Check, database.oauthClients.Check, database.oauthCodes.Check, database.grants.Check,
		database.spentTokens.Check,
	}

	for _, check := range checks {
		found, err := check(database.db)

		if err != nil {
			return problems, err
		}

		problems = append(problems, found...)
	}

	counted := map[string]int64{}

	for _, count := range []func(kv.Reader) (map[string]int64, error){database.logs.Counted, database.events.Counted, database.issues.Counted} {
		sizes, err := count(database.db)

		if err != nil {
			return problems, err
		}

		for counter, size := range sizes {
			counted[counter] += size
		}
	}

	if stored := database.db.Counters(); !maps.Equal(stored, counted) {
		problems = append(problems, fmt.Sprintf("byte counters %v, records take %v", stored, counted))
	}

	database.issuesMutex.RLock()

	issues := make([]issueRecord, 0, len(database.table.byID))

	for _, issue := range database.table.byID {
		issues = append(issues, *issue)
	}

	database.issuesMutex.RUnlock()

	for _, issue := range issues {
		stored, _, err := database.events.Primary(kv.Text(issue.ID)).Count(database.db, 0)

		if err != nil {
			return problems, err
		}

		days := 0

		for _, entry := range issue.EventDays {
			days += entry.Count
		}

		if stored != issue.Stored || days != issue.Stored {
			problems = append(problems, fmt.Sprintf("issue %s counts %d events (%d by day), %d stored", issue.ID, issue.Stored, days, stored))
		}
	}

	return problems, nil
}
