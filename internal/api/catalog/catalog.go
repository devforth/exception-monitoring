package catalog

import (
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/services"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// handlers holds what the handlers read; one per server.
type handlers struct {
	store    store.Store
	projects config.ProjectList
	services *services.All
}

// New is the catalog, in the order agents and the UI list the tools. To add
// an endpoint, add it here with its handler: REST and MCP pick it up.
// Descriptions say when to use an endpoint; the MCP instructions and skills
// hold the workflow. Called with nil arguments it serves only to read the
// definitions (the MCP page lists them).
func New(database store.Store, projects config.ProjectList, all *services.All) []Endpoint {
	handler := &handlers{store: database, projects: projects, services: all}

	return append([]Endpoint{
		Define(Endpoint{
			Name: "list_issues",
			Description: "List issues (errors grouped by cause) with event count and first/last seen. " +
				"Default: unresolved, most recently seen first, every project. The way to find errors without known IDs.",
			ReadOnly: true,
		}, handler.listIssues),
		Define(Endpoint{
			Name: "get_issue",
			Description: "Diagnose an issue: its latest event (or event_id) as the agent prompt of notifications, with the exception chain, " +
				"stack frames with source, message, request, runtime, tags, breadcrumbs, up to 20 logs of the request and the issue history.",
			ReadOnly: true,
		}, handler.getIssue),
		Define(Endpoint{
			Name:        "get_issues",
			Description: "Summaries and current statuses of 1 to 100 known issue IDs in one call, without event details.",
			ReadOnly:    true,
		}, handler.getIssues),
		Define(Endpoint{
			Name:        "list_issue_events",
			Description: "List the stored events of an issue, newest first: time, level, environment, release and trace ID. Pass an event_id to get_issue to read one.",
			ReadOnly:    true,
		}, handler.listIssueEvents),
		Define(Endpoint{
			Name:        "get_event",
			Description: "Like get_issue, for an event ID the SDK reported to the application (with its project).",
			ReadOnly:    true,
		}, handler.getEvent),
		Define(Endpoint{
			Name:        "list_logs",
			Description: "Search logs, newest first. Pass trace_id (from get_issue) for the logs of one request or browser session.",
			ReadOnly:    true,
		}, handler.listLogs),
		Define(Endpoint{
			Name: "list_projects",
			Description: "List the monitored projects with their unresolved issue counts and DSNs (what a Sentry SDK is configured with to report to the project). " +
				"Not needed before list_issues: issue rows name their project.",
			ReadOnly: true,
		}, handler.listProjects),
		Define(Endpoint{
			Name: "update_issue_status",
			Description: "Set the status of 1 to 100 issues in one atomic call: resolved (fixed; reopens if the error happens again), " +
				"ignored (no notifications, stays ignored) or unresolved. Only when the user asks. Returns the updated summaries.",
			Idempotent: true,
		}, handler.updateIssueStatus),

		Define(Endpoint{
			Name:        "delete_issue",
			Description: "Delete an issue with its events; if the error happens again it comes back as a new issue. Admins only.",
			Destructive: true,
			Admin:       true,
		}, handler.deleteIssue),

		// REST only.
		Define(Endpoint{
			Name:        "get_event_payload",
			Description: "An event with the full JSON the SDK sent (credentials removed).",
			ReadOnly:    true,
			RESTOnly:    true,
		}, handler.getEventPayload),
		Define(Endpoint{
			Name:        "list_trace",
			Description: "Events (without payloads) and logs of one trace, up to 100 of each.",
			ReadOnly:    true,
			RESTOnly:    true,
		}, handler.listTrace),
	}, handler.management()...)
}
