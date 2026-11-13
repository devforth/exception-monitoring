// Package presenters turns stored data into the view types of contracts:
// table rows, forms filled from a record, display wording. Presenters do no
// I/O (no store, no services): a controller fetches the data through
// services, hands it to a presenter and renders the result. What they need
// besides the data (settings, project names, colors) comes as read-only
// fields, so tests build them from plain values.
package presenters

import (
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
)

// ProjectName names a project by id, also one that no longer exists
// (services.Projects.Name).
type ProjectName func(projectID int) string

// IssueRows labels each issue with its project.
func IssueRows(issues []store.Issue, projectName ProjectName) []contracts.IssueRow {
	rows := make([]contracts.IssueRow, 0, len(issues))

	for _, issue := range issues {
		rows = append(rows, IssueRow(issue, projectName))
	}

	return rows
}

func IssueRow(issue store.Issue, projectName ProjectName) contracts.IssueRow {
	return contracts.IssueRow{Issue: issue, ProjectName: projectName(issue.ProjectID)}
}

// LogRows labels each log with its project and the issue raised in the
// same request (issues, by log id).
func LogRows(logs []store.LogRecord, issues map[int64]store.LogIssue, projectName ProjectName) []contracts.LogRow {
	rows := make([]contracts.LogRow, 0, len(logs))

	for _, record := range logs {
		row := contracts.LogRow{LogRecord: record, ProjectName: projectName(record.ProjectID)}

		if issue, ok := issues[record.ID]; ok {
			row.Issue = &issue
		}

		rows = append(rows, row)
	}

	return rows
}
