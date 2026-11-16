package catalog

import (
	"context"
	"errors"
	"net/url"
	"strconv"
	"time"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/services"
	"github.com/devforth/exception-monitoring/internal/storage/projects"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// The endpoints that manage the server, as the UI pages do: they act as a
// user (AgentsOnly) and go through the same services, which check what the
// user may change. A user changes their own password only in the UI, where
// the current one is asked; an agent never mints MCP tokens.
func (handler *handlers) management() []Endpoint {
	return append([]Endpoint{
		Define(Endpoint{
			Name:        "get_account",
			Description: "Who the agent acts for: username, name and role (superadmin, admin or user), which decides the tools offered.",
			ReadOnly:    true, AgentsOnly: true,
		}, handler.getAccount),
		Define(Endpoint{
			Name:        "update_account_name",
			Description: "Change the display name of the user the agent acts for. The password can only be changed in the UI.",
			Idempotent:  true, AgentsOnly: true,
		}, handler.updateAccountName),
		Define(Endpoint{
			Name:        "list_agents",
			Description: "Coding agents and personal tokens connected to MCP for the user (admins: user_id of a user they manage), with when each was last used.",
			ReadOnly:    true, AgentsOnly: true,
		}, handler.listAgents),
		Define(Endpoint{
			Name:        "disconnect_agent",
			Description: "Revoke a connected agent or personal token (grant_id from list_agents); it stops working at once.",
			Destructive: true, AgentsOnly: true,
		}, handler.disconnectAgent),

		Define(Endpoint{
			Name:        "get_project",
			Description: "Settings of a project: DSN, color, recorded levels and retention, empty values following the server defaults.",
			ReadOnly:    true, Admin: true, AgentsOnly: true,
		}, handler.getProject),
		Define(Endpoint{
			Name:        "create_project",
			Description: "Add a project (one per service sending errors); returns its DSN for the app's Sentry SDK.",
			Admin:       true, AgentsOnly: true,
		}, handler.createProject),
		Define(Endpoint{
			Name:        "update_project",
			Description: "Change a project's settings; fields not given keep their values, an empty string returns a setting to the default. Projects from PROJECTS keep their name.",
			Idempotent:  true, Admin: true, AgentsOnly: true, Confirm: true,
		}, handler.updateProject),
		Define(Endpoint{
			Name:        "delete_project",
			Description: "Delete a project added in the UI with all its issues, events and logs; apps using its DSN are rejected afterwards.",
			Destructive: true, Admin: true, AgentsOnly: true,
		}, handler.deleteProject),

		Define(Endpoint{
			Name:        "list_users",
			Description: "Users of the panel with their role and last login.",
			ReadOnly:    true, Admin: true, AgentsOnly: true,
		}, handler.listUsers),
		Define(Endpoint{
			Name:        "create_user",
			Description: "Add a user with a role (admin or user). Returns a temporary password, shown only this once, that the user must change at the first login: give it to the user privately.",
			Admin:       true, AgentsOnly: true, Confirm: true,
		}, handler.createUser),
		Define(Endpoint{
			Name:        "update_user",
			Description: "Change another user's name or role (admin or user); fields not given keep their values. Not the superadmin, not yourself (update_account_name).",
			Idempotent:  true, Admin: true, AgentsOnly: true, Confirm: true,
		}, handler.updateUser),
		Define(Endpoint{
			Name:        "reset_user_password",
			Description: "Give another user a new temporary password (returned once): their sessions end and their connected agents are disconnected.",
			Destructive: true, Admin: true, AgentsOnly: true,
		}, handler.resetUserPassword),
		Define(Endpoint{
			Name:        "delete_user",
			Description: "Delete another user; their sessions and connected agents end. Their past status changes keep their name.",
			Destructive: true, Admin: true, AgentsOnly: true,
		}, handler.deleteUser),
	}, handler.notificationEndpoints()...)
}

// Account and agents.

type userOutput struct {
	ID                 int64      `json:"id"`
	Username           string     `json:"username"`
	Name               string     `json:"name,omitempty"`
	Role               string     `json:"role"`
	MustChangePassword bool       `json:"must_change_password,omitempty"`
	LastLoginAt        *time.Time `json:"last_login_at,omitempty"`
}

func userView(user store.User) userOutput {
	return userOutput{ID: user.ID, Username: user.Username, Name: user.Name, Role: user.Role, MustChangePassword: user.MustChangePassword, LastLoginAt: user.LastLoginAt}
}

type noInput struct{}

func (handler *handlers) getAccount(_ context.Context, call Call, _ noInput) (userOutput, error) {
	return userView(call.Viewer), nil
}

type accountNameInput struct {
	Name string `json:"name" jsonschema:"The new display name; empty shows the username."`
}

func (handler *handlers) updateAccountName(ctx context.Context, call Call, input accountNameInput) (userOutput, error) {
	problems, err := handler.services.Users.UpdateName(ctx, call.Viewer, input.Name)

	if err != nil {
		return userOutput{}, err
	}

	if err := problemsError(problems, nil); err != nil {
		return userOutput{}, err
	}

	user, err := handler.services.Users.Get(ctx, call.Viewer.ID)

	return userView(user), err
}

type listAgentsInput struct {
	UserID int64 `json:"user_id,omitempty" jsonschema:"Admins: a user they manage; the user the agent acts for when empty."`
}

type agentOutput struct {
	GrantID    int64      `json:"grant_id"`
	Name       string     `json:"name"`
	Kind       string     `json:"kind"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

type agentsOutput struct {
	User   string        `json:"user"`
	Agents []agentOutput `json:"agents"`
}

func (handler *handlers) listAgents(ctx context.Context, call Call, input listAgentsInput) (agentsOutput, error) {
	page, err := handler.services.Agents.Page(ctx, call.Viewer, input.UserID)

	if err != nil {
		return agentsOutput{}, err
	}

	// Page falls back to the viewer's own agents: say so instead.
	if input.UserID != 0 && page.Owner.ID != input.UserID {
		return agentsOutput{}, ErrNotFound
	}

	output := agentsOutput{User: page.Owner.Username, Agents: []agentOutput{}}

	for _, grant := range page.Grants {
		output.Agents = append(output.Agents, agentOutput{GrantID: grant.ID, Name: call.Text(grant.Name), Kind: grant.Kind, CreatedAt: grant.CreatedAt, LastUsedAt: grant.LastUsedAt})
	}

	return output, nil
}

type grantInput struct {
	GrantID int64 `json:"grant_id" jsonschema:"grant_id from list_agents."`
}

type doneOutput struct {
	Done string `json:"done"`
}

func (handler *handlers) disconnectAgent(ctx context.Context, call Call, input grantInput) (doneOutput, error) {
	if err := handler.services.Agents.Disconnect(ctx, call.Viewer, input.GrantID); err != nil {
		return doneOutput{}, err
	}

	return doneOutput{Done: "disconnected"}, nil
}

// Projects.

type projectInput struct {
	Project string `json:"project" jsonschema:"Project name or ID."`
}

type projectSettings struct {
	ID                      int      `json:"id"`
	Name                    string   `json:"name"`
	DSN                     string   `json:"dsn,omitempty"`
	FromEnv                 bool     `json:"from_env,omitempty"`
	Color                   string   `json:"color,omitempty"`
	EventLevels             []string `json:"event_levels"`
	LogLevels               []string `json:"log_levels"`
	RetentionEvents         string   `json:"retention_events,omitempty"`
	RetentionLogs           string   `json:"retention_logs,omitempty"`
	RetentionIssues         string   `json:"retention_issues,omitempty"`
	RetentionEventsPerIssue string   `json:"retention_events_per_issue,omitempty"`
	RetentionLogsPerMessage string   `json:"retention_logs_per_message,omitempty"`
}

// findProject reads the project given by name or ID.
func (handler *handlers) findProject(ctx context.Context, nameOrID string) (store.Project, error) {
	project, ok := handler.projects.List().Find(nameOrID)

	if !ok {
		return store.Project{}, Invalid("unknown project %q (see list_projects)", nameOrID)
	}

	return handler.services.Projects.Get(ctx, project.ID)
}

// settings shows a project the way update_project takes it (its form).
func (handler *handlers) settings(call Call, project store.Project) projectSettings {
	form := handler.services.Projects.FormValues(project)
	output := projectSettings{
		ID: project.ID, Name: project.Name, FromEnv: handler.services.Projects.FromEnv(project.ID), Color: project.Color,
		EventLevels: form["event_level"], LogLevels: form["log_level"],
		RetentionEvents: form.Get("retention_events"), RetentionLogs: form.Get("retention_logs"), RetentionIssues: form.Get("retention_issues"),
		RetentionEventsPerIssue: form.Get("retention_events_per_issue"), RetentionLogsPerMessage: form.Get("retention_logs_per_message"),
	}

	if call.Address != nil {
		output.DSN = config.Project{ID: project.ID, PublicKey: project.PublicKey}.DSN(call.Address)
	}

	return output
}

func (handler *handlers) getProject(ctx context.Context, call Call, input projectInput) (projectSettings, error) {
	project, err := handler.findProject(ctx, input.Project)

	if err != nil {
		return projectSettings{}, err
	}

	return handler.settings(call, project), nil
}

// projectFields are the settings a tool may change; nil keeps the value.
type projectFields struct {
	Color                   *string   `json:"color,omitempty" jsonschema:"#rrggbb, or empty for the palette color."`
	EventLevels             *[]string `json:"event_levels,omitempty" jsonschema:"Event levels to store: fatal, error, warning, info, debug. An empty list stores none."`
	LogLevels               *[]string `json:"log_levels,omitempty" jsonschema:"Log levels to store: fatal, error, warn, info, debug, trace. An empty list stores none."`
	RetentionEvents         *string   `json:"retention_events,omitempty" jsonschema:"Delete events older than this (30d, 12h; 0 keeps them); empty for the default."`
	RetentionLogs           *string   `json:"retention_logs,omitempty" jsonschema:"Delete logs older than this; empty for the default."`
	RetentionIssues         *string   `json:"retention_issues,omitempty" jsonschema:"Delete issues not seen for this long; empty for the default."`
	RetentionEventsPerIssue *string   `json:"retention_events_per_issue,omitempty" jsonschema:"Newest events kept per issue (0 keeps all); empty for the default."`
	RetentionLogsPerMessage *string   `json:"retention_logs_per_message,omitempty" jsonschema:"Newest logs kept per repeated message (0 keeps all); empty for the default."`
}

// apply writes the given fields over the form.
func (fields projectFields) apply(form url.Values) {
	for name, value := range map[string]*string{
		"color": fields.Color, "retention_events": fields.RetentionEvents, "retention_logs": fields.RetentionLogs,
		"retention_issues": fields.RetentionIssues, "retention_events_per_issue": fields.RetentionEventsPerIssue,
		"retention_logs_per_message": fields.RetentionLogsPerMessage,
	} {
		if value != nil {
			form.Set(name, *value)
		}
	}

	if fields.EventLevels != nil {
		form["event_level"] = *fields.EventLevels
	}

	if fields.LogLevels != nil {
		form["log_level"] = *fields.LogLevels
	}
}

// projectFormFields names the form's fields as the tools do.
var projectFormFields = map[string]string{"event_level": "event_levels", "log_level": "log_levels"}

type createProjectInput struct {
	Name string `json:"name" jsonschema:"Lowercase letters, digits, '-' and '_' (backend, frontend, worker)."`
	projectFields
}

func (handler *handlers) saveProject(ctx context.Context, call Call, form url.Values, project store.Project) (projectSettings, error) {
	saved, problems, err := handler.services.Projects.Save(ctx, form, project)

	if err != nil {
		return projectSettings{}, err
	}

	if err := problemsError(problems, projectFormFields); err != nil {
		return projectSettings{}, err
	}

	return handler.settings(call, saved), nil
}

func (handler *handlers) createProject(ctx context.Context, call Call, input createProjectInput) (projectSettings, error) {
	form := handler.services.Projects.FormValues(store.Project{})
	form.Set("name", input.Name)
	input.apply(form)

	return handler.saveProject(ctx, call, form, store.Project{})
}

type updateProjectInput struct {
	Project string  `json:"project" jsonschema:"Project name or ID."`
	Name    *string `json:"name,omitempty" jsonschema:"A new name (not for projects from PROJECTS)."`
	projectFields
}

func (handler *handlers) updateProject(ctx context.Context, call Call, input updateProjectInput) (projectSettings, error) {
	project, err := handler.findProject(ctx, input.Project)

	if err != nil {
		return projectSettings{}, err
	}

	form := handler.services.Projects.FormValues(project)

	if input.Name != nil {
		form.Set("name", *input.Name)
	}

	input.apply(form)

	return handler.saveProject(ctx, call, form, project)
}

func (handler *handlers) deleteProject(ctx context.Context, _ Call, input projectInput) (doneOutput, error) {
	project, err := handler.findProject(ctx, input.Project)

	if err != nil {
		return doneOutput{}, err
	}

	if err := handler.services.Projects.Delete(ctx, project.ID); errors.Is(err, projects.ErrFromEnv) {
		return doneOutput{}, Invalid("project %q comes from PROJECTS: remove it there", project.Name)
	} else if err != nil {
		return doneOutput{}, err
	}

	return doneOutput{Done: "deleted project " + project.Name}, nil
}

// Users.

func (handler *handlers) listUsers(ctx context.Context, _ Call, _ noInput) (Page[userOutput], error) {
	users, err := handler.services.Users.List(ctx)
	output := Page[userOutput]{Items: []userOutput{}}

	for _, user := range users {
		output.Items = append(output.Items, userView(user))
	}

	return output, err
}

type createUserInput struct {
	Username string `json:"username" jsonschema:"Login name: letters, digits and . _ @ -."`
	Name     string `json:"name,omitempty" jsonschema:"Display name."`
	Role     string `json:"role" jsonschema:"admin or user."`
}

type passwordOutput struct {
	User              userOutput `json:"user"`
	TemporaryPassword string     `json:"temporary_password"`
	Note              string     `json:"note"`
}

const temporaryPasswordNote = "Shown only this once. Give it to the user privately; they choose their own password at the first login. Do not store it."

func (handler *handlers) createUser(ctx context.Context, call Call, input createUserInput) (passwordOutput, error) {
	id, password, problems, err := handler.services.Users.Create(ctx, call.Viewer, services.UserInput{Username: input.Username, Name: input.Name, Role: input.Role})

	if err != nil {
		return passwordOutput{}, err
	}

	if err := problemsError(problems, nil); err != nil {
		return passwordOutput{}, err
	}

	return handler.withPassword(ctx, id, password)
}

func (handler *handlers) withPassword(ctx context.Context, id int64, password string) (passwordOutput, error) {
	user, err := handler.services.Users.Get(ctx, id)

	return passwordOutput{User: userView(user), TemporaryPassword: password, Note: temporaryPasswordNote}, err
}

type userInput struct {
	UserID int64 `json:"user_id" jsonschema:"id from list_users."`
}

type updateUserInput struct {
	UserID int64   `json:"user_id" jsonschema:"id from list_users."`
	Name   *string `json:"name,omitempty" jsonschema:"Display name."`
	Role   *string `json:"role,omitempty" jsonschema:"admin or user."`
}

func (handler *handlers) updateUser(ctx context.Context, call Call, input updateUserInput) (userOutput, error) {
	user, err := handler.services.Users.Get(ctx, input.UserID)

	if err != nil {
		return userOutput{}, err
	}

	change := services.UserInput{Username: user.Username, Name: user.Name, Role: user.Role}

	if input.Name != nil {
		change.Name = *input.Name
	}

	if input.Role != nil {
		change.Role = *input.Role
	}

	problems, err := handler.services.Users.Update(ctx, call.Viewer, input.UserID, change)

	if err != nil {
		return userOutput{}, err
	}

	if err := problemsError(problems, nil); err != nil {
		return userOutput{}, err
	}

	user, err = handler.services.Users.Get(ctx, input.UserID)

	return userView(user), err
}

func (handler *handlers) resetUserPassword(ctx context.Context, call Call, input userInput) (passwordOutput, error) {
	password, err := handler.services.Users.ResetPassword(ctx, call.Viewer, input.UserID)

	if err != nil {
		return passwordOutput{}, err
	}

	return handler.withPassword(ctx, input.UserID, password)
}

func (handler *handlers) deleteUser(ctx context.Context, call Call, input userInput) (doneOutput, error) {
	if err := handler.services.Users.Delete(ctx, call.Viewer, input.UserID); err != nil {
		return doneOutput{}, err
	}

	return doneOutput{Done: "deleted user " + strconv.FormatInt(input.UserID, 10)}, nil
}
