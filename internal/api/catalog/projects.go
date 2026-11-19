package catalog

import (
	"context"
	"strconv"
)

type listProjectsInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"Projects per page, 1 to 100 (default 20)."`
	Cursor string `json:"cursor,omitempty" jsonschema:"next_cursor of the previous page."`
}

type projectOutput struct {
	ID               int    `json:"id"`
	Name             string `json:"name"`
	UnresolvedIssues int    `json:"unresolved_issues"`
	// DSN configures a Sentry SDK to report to the project.
	DSN string `json:"dsn,omitempty"`
}

func (handler *handlers) listProjects(ctx context.Context, call Call, input listProjectsInput) (Page[projectOutput], error) {
	output := Page[projectOutput]{Items: []projectOutput{}}
	limit, err := parseLimit(input.Limit)

	if err != nil {
		return output, err
	}

	offset, err := parseCursor(input.Cursor)

	if err != nil {
		return output, err
	}

	projects := handler.projects.List()

	if offset >= int64(len(projects)) {
		return output, nil
	}

	counts, err := handler.services.Projects.UnresolvedCounts(ctx)

	if err != nil {
		return output, err
	}

	end := min(int(offset)+limit, len(projects))

	for _, project := range projects[int(offset):end] {
		item := projectOutput{ID: project.ID, Name: project.Name, UnresolvedIssues: counts[project.ID]}

		if call.Address != nil {
			item.DSN = project.DSN(call.Address)
		}

		output.Items = append(output.Items, item)
	}

	if end < len(projects) {
		output.NextCursor = strconv.Itoa(end)
	}

	return output, nil
}
