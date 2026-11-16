package endpoints

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/store"
)

type listLogsInput struct {
	Project string `json:"project,omitempty" jsonschema:"Project name or ID; every project when empty."`
	Level   string `json:"level,omitempty" jsonschema:"fatal, error, warn, info, debug or trace."`
	Query   string `json:"query,omitempty" jsonschema:"Text in the log line."`
	TraceID string `json:"trace_id,omitempty" jsonschema:"Only logs of this trace (see get_issue)."`
	Since   string `json:"since,omitempty" jsonschema:"RFC 3339 time: only logs at or after it."`
	Until   string `json:"until,omitempty" jsonschema:"RFC 3339 time: only logs before it."`
	Limit   int    `json:"limit,omitempty" jsonschema:"Logs per page, 1 to 100 (default 20)."`
	Cursor  string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page."`
}

type logOutput struct {
	Time    time.Time `json:"time"`
	Project string    `json:"project"`
	Level   string    `json:"level"`
	Body    string    `json:"body"`
	TraceID string    `json:"trace_id,omitempty"`
}

func (handler *handlers) listLogs(ctx context.Context, call Call, input listLogsInput) (Page[logOutput], error) {
	output := Page[logOutput]{Items: []logOutput{}}
	filter, err := handler.logFilter(input)

	if err != nil {
		return output, err
	}

	limit := filter.Limit
	filter.Limit++
	logs, err := handler.store.ListLogs(ctx, filter)

	if err != nil {
		return output, err
	}

	logs, more := store.Page(logs, limit)

	for _, record := range logs {
		output.Items = append(output.Items, logOutput{
			Time: record.Timestamp, Project: handler.projectName(record.ProjectID), Level: record.Level,
			Body: call.Text(record.Body), TraceID: record.TraceID,
		})
	}

	if more {
		output.NextCursor = strconv.FormatInt(logs[limit-1].ID, 10)
	}

	return output, nil
}

func (handler *handlers) logFilter(input listLogsInput) (store.LogFilter, error) {
	filter := store.LogFilter{Query: strings.TrimSpace(input.Query), TraceID: strings.TrimSpace(input.TraceID)}

	var err error

	if filter.ProjectIDs, err = projectIDs(handler.projects.List(), input.Project); err != nil {
		return filter, err
	}

	if filter.Levels, err = levels(input.Level, config.LogLevels); err != nil {
		return filter, err
	}

	if filter.Since, err = parseTime("since", input.Since); err != nil {
		return filter, err
	}

	if filter.Until, err = parseTime("until", input.Until); err != nil {
		return filter, err
	}

	if filter.Limit, err = parseLimit(input.Limit); err != nil {
		return filter, err
	}

	filter.BeforeID, err = parseCursor(input.Cursor)

	return filter, err
}

type listTraceInput struct {
	TraceID string `json:"trace_id" jsonschema:"Trace ID: 32 hex characters."`
}

type traceOutput struct {
	TraceID string               `json:"trace_id"`
	Events  []store.EventSummary `json:"events"`
	Logs    []store.LogRecord    `json:"logs"`
}

// listTrace is REST only: log attributes are raw SDK data.
func (handler *handlers) listTrace(ctx context.Context, _ Call, input listTraceInput) (traceOutput, error) {
	traceID := strings.TrimSpace(input.TraceID)

	if traceID == "" {
		return traceOutput{}, Invalid("trace_id is required")
	}

	output := traceOutput{TraceID: traceID}
	events, err := handler.store.ListEvents(ctx, store.EventFilter{TraceID: traceID, Limit: MaxLimit})

	if err != nil {
		return output, err
	}

	logs, err := handler.store.ListLogs(ctx, store.LogFilter{TraceID: traceID, Limit: MaxLimit})

	if err != nil {
		return output, err
	}

	output.Events, output.Logs = append([]store.EventSummary{}, events...), append([]store.LogRecord{}, logs...)

	return output, nil
}
