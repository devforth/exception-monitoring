package mcp

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/devforth/exception-monitoring/internal/store"
)

const (
	defaultLimit = 20
	maxLimit     = 100
)

var readOnly = &sdk.ToolAnnotations{ReadOnlyHint: true}

// The tools, in the order agents and the UI list them. Descriptions say when
// to use a tool; the server instructions and skills hold the workflow.
var (
	listProjectsTool = &sdk.Tool{
		Name:        "list_projects",
		Description: "List the monitored projects with their unresolved issue counts. Not needed before list_issues: issue rows name their project.",
		Annotations: readOnly,
	}
	listIssuesTool = &sdk.Tool{
		Name: "list_issues",
		Description: "List issues (errors grouped by cause) with event count and first/last seen. " +
			"Default: unresolved, most recently seen first, every project. The way to find errors without known IDs.",
		Annotations: readOnly,
	}
	getIssuesTool = &sdk.Tool{
		Name:        "get_issues",
		Description: "Summaries and current statuses of 1 to 100 known issue IDs in one call, without event details.",
		Annotations: readOnly,
	}
	getIssueTool = &sdk.Tool{
		Name: "get_issue",
		Description: "Diagnose an issue: its latest event (or event_id) as the agent prompt of notifications, with the exception chain, " +
			"stack frames with source, message, request, runtime, tags, breadcrumbs, up to 20 logs of the request and the issue history.",
		Annotations: readOnly,
	}
	getEventTool = &sdk.Tool{
		Name:        "get_event",
		Description: "Like get_issue, for an event ID the SDK reported to the application (with its project).",
		Annotations: readOnly,
	}
	listIssueEventsTool = &sdk.Tool{
		Name:        "list_issue_events",
		Description: "List the stored events of an issue, newest first: time, level, environment, release and trace ID. Pass an event_id to get_issue to read one.",
		Annotations: readOnly,
	}
	listLogsTool = &sdk.Tool{
		Name:        "list_logs",
		Description: "Search logs, newest first. Pass trace_id (from get_issue) for the logs of one request or browser session.",
		Annotations: readOnly,
	}
	updateIssueStatusTool = &sdk.Tool{
		Name: "update_issue_status",
		Description: "Set the status of 1 to 100 issues in one atomic call: resolved (fixed; reopens if the error happens again), " +
			"ignored (no notifications, stays ignored) or unresolved. Only when the user asks. Returns the updated summaries.",
		Annotations: &sdk.ToolAnnotations{IdempotentHint: true, DestructiveHint: new(false)},
	}
	tools = []*sdk.Tool{
		listIssuesTool, getIssueTool, getIssuesTool, listIssueEventsTool, getEventTool, listLogsTool, listProjectsTool,
		updateIssueStatusTool, fetchSkillTool,
	}
)

// ToolInfo describes a tool for people (the MCP page of the UI).
type ToolInfo struct {
	Name        string
	Description string
	ReadOnly    bool
}

// Tools lists the tools the server offers.
func Tools() []ToolInfo {
	list := make([]ToolInfo, 0, len(tools))

	for _, tool := range tools {
		list = append(list, ToolInfo{Name: tool.Name, Description: tool.Description, ReadOnly: tool.Annotations.ReadOnlyHint})
	}

	return list
}

func (handler *Handler) addTools(server *sdk.Server) {
	sdk.AddTool(server, fetchSkillTool, handler.fetchSkill)
	sdk.AddTool(server, listProjectsTool, handler.listProjects)
	sdk.AddTool(server, listIssuesTool, handler.listIssues)
	sdk.AddTool(server, getIssueTool, handler.getIssue)
	sdk.AddTool(server, getEventTool, handler.getEvent)
	sdk.AddTool(server, listIssueEventsTool, handler.listIssueEvents)
	sdk.AddTool(server, listLogsTool, handler.listLogs)
	sdk.AddTool(server, getIssuesTool, handler.getIssues)
	sdk.AddTool(server, updateIssueStatusTool, handler.updateIssueStatus)
}

// errNotFound is what the agent reads about a missing issue or event.
var errNotFound = errors.New("not found")

// result returns value as JSON text.
func result(value any) (*sdk.CallToolResult, any, error) {
	encoded, err := json.Marshal(value)

	if err != nil {
		return nil, nil, err
	}

	return text(string(encoded))
}

func text(content string) (*sdk.CallToolResult, any, error) {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: content}}}, nil, nil
}

// failed turns an error into what the agent reads: input and lookup errors as
// they are, anything else logged and reported without details.
func (handler *Handler) failed(tool string, err error) (*sdk.CallToolResult, any, error) {
	var input inputError

	switch {
	case errors.As(err, &input), errors.Is(err, errNotFound):
		return nil, nil, err
	case errors.Is(err, store.ErrNotFound):
		return nil, nil, errNotFound
	default:
		handler.logger.Error("mcp tool failed", "tool", tool, "err", err)

		return nil, nil, errors.New("internal error")
	}
}

type inputError struct{ message string }

func (err inputError) Error() string { return err.message }

func invalid(format string, args ...any) error {
	return inputError{fmt.Sprintf(format, args...)}
}

type page[T any] struct {
	Items      []T    `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// projectName is empty for data of a deleted project.
func (handler *Handler) projectName(projectID int) string {
	project, _ := handler.projects.List().ByID(projectID)

	return project.Name
}

// projectIDs resolves a project given by name or ID as a filter: none when empty.
func (handler *Handler) projectIDs(nameOrID string) ([]int, error) {
	if nameOrID = strings.TrimSpace(nameOrID); nameOrID == "" {
		return nil, nil
	}

	project, ok := handler.projects.List().Find(nameOrID)

	if !ok {
		return nil, invalid("unknown project %q (see list_projects)", nameOrID)
	}

	return []int{project.ID}, nil
}

// levels checks a level filter against the known levels: none when empty.
func levels(level string, known []string) ([]string, error) {
	if level == "" {
		return nil, nil
	}

	if !slices.Contains(known, level) {
		return nil, invalid("level must be one of %s", strings.Join(known, ", "))
	}

	return []string{level}, nil
}

func parseTime(name, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}

	parsed, err := time.Parse(time.RFC3339Nano, value)

	if err != nil {
		return time.Time{}, invalid("%s must be an RFC 3339 time, e.g. 2026-11-11T10:00:00Z", name)
	}

	return parsed, nil
}

func parseLimit(limit int) (int, error) {
	if limit == 0 {
		return defaultLimit, nil
	}

	if limit < 1 || limit > maxLimit {
		return 0, invalid("limit must be between 1 and %d", maxLimit)
	}

	return limit, nil
}

// Cursors are plain numbers: an offset for issues (sorted by fields that
// change), the last ID seen for events and logs (newest first by ID).
func parseCursor(cursor string) (int64, error) {
	if cursor == "" {
		return 0, nil
	}

	value, err := strconv.ParseInt(cursor, 10, 64)

	if err != nil || value < 0 {
		return 0, invalid("invalid cursor: pass next_cursor of the previous page")
	}

	return value, nil
}
