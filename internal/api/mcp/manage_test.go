package mcp_test

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/devforth/exception-monitoring/internal/mcp"
	"github.com/devforth/exception-monitoring/internal/store"
)

// as is the server seen by an agent of a new user with role.
func (server *testServer) as(role string) (*testServer, int64) {
	server.t.Helper()

	ctx := context.Background()
	id, err := server.database.CreateUser(ctx, store.User{Username: role + "-user", Role: role, CreatedAt: time.Now()}, "x")

	if err != nil {
		server.t.Fatal(err)
	}

	token, err := server.authorization.NewPersonalToken(ctx, id)

	if err != nil {
		server.t.Fatal(err)
	}

	other := *server
	other.token, other.session = token, nil

	return &other, id
}

func toolNames(t *testing.T, session *sdk.ClientSession) []string {
	t.Helper()

	var names []string

	for tool, err := range session.Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}

		names = append(names, tool.Name)
	}

	return names
}

// An agent is offered only what its user's role allows, and the role is
// read at every request.
func TestToolsFollowTheRole(t *testing.T) {
	server := newTestServer(t)
	admin := toolNames(t, server.connect())

	for _, name := range []string{"create_user", "delete_issue", "create_connection", "update_project", "get_account"} {
		if !slices.Contains(admin, name) {
			t.Errorf("superadmin is not offered %s", name)
		}
	}

	// Never a tool: the own password (UI only) and minting MCP tokens.
	for _, name := range []string{"change_password", "update_account_password", "generate_personal_token", "get_event_payload", "list_trace"} {
		if slices.Contains(admin, name) {
			t.Errorf("%s offered to agents", name)
		}
	}

	user, userID := server.as(store.RoleUser)
	names := toolNames(t, user.connect())

	var listed []string

	for _, tool := range mcp.Tools(store.User{ID: userID, Role: store.RoleUser}) {
		listed = append(listed, tool.Name)
	}

	slices.Sort(names)
	slices.Sort(listed)

	if !slices.Equal(names, listed) {
		t.Errorf("user tools %v, mcp.Tools lists %v", names, listed)
	}

	for _, name := range []string{"create_user", "list_users", "delete_issue", "create_connection", "update_project", "get_project"} {
		if slices.Contains(names, name) {
			t.Errorf("user is offered %s", name)
		}
	}

	for _, name := range []string{"get_account", "update_account_name", "list_agents", "update_issue_status"} {
		if !slices.Contains(names, name) {
			t.Errorf("user is not offered %s", name)
		}
	}

	// Calling an admin tool anyway does not run it.
	if result, err := user.connect().CallTool(context.Background(), &sdk.CallToolParams{Name: "create_user", Arguments: map[string]any{"username": "x", "role": "admin"}}); err == nil && !result.IsError {
		t.Fatal("a user's agent created a user")
	}

	if _, err := server.database.FindUser(context.Background(), "x"); err == nil {
		t.Fatal("the user was created")
	}

	// Made an admin: the next request offers the admin tools.
	if err := server.database.UpdateUser(context.Background(), userID, "", store.RoleAdmin); err != nil {
		t.Fatal(err)
	}

	if names := toolNames(t, user.connect()); !slices.Contains(names, "create_user") {
		t.Errorf("tools after promotion: %v", names)
	}
}

func TestDestructiveToolsAskForConfirmation(t *testing.T) {
	server := newTestServer(t)
	destructive := 0

	for tool, err := range server.connect().Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}

		if tool.Annotations.DestructiveHint != nil && *tool.Annotations.DestructiveHint {
			destructive++

			if !strings.Contains(tool.Description, "explicit confirmation") {
				t.Errorf("%s: %s", tool.Name, tool.Description)
			}
		}
	}

	if destructive < 5 {
		t.Fatalf("%d destructive tools", destructive)
	}

	// Changes of who may access the server or where errors go are confirmed too.
	for tool, err := range server.connect().Tools(context.Background(), nil) {
		if err != nil {
			t.Fatal(err)
		}

		sensitive := slices.Contains([]string{"create_user", "update_user", "create_connection", "update_connection", "update_project"}, tool.Name)

		if sensitive != strings.Contains(tool.Description, "Changes who can access") {
			t.Errorf("%s: %s", tool.Name, tool.Description)
		}
	}
}

type projectSettings struct {
	ID            int      `json:"id"`
	Name          string   `json:"name"`
	DSN           string   `json:"dsn"`
	Color         string   `json:"color"`
	EventLevels   []string `json:"event_levels"`
	RetentionLogs string   `json:"retention_logs"`
}

func TestManageProjects(t *testing.T) {
	server := newTestServer(t)

	var created projectSettings
	server.decode("create_project", map[string]any{"name": "worker", "retention_logs": "7d", "event_levels": []string{"error"}}, &created)

	if created.ID == 0 || !strings.Contains(created.DSN, "/"+strconv.Itoa(created.ID)) || created.RetentionLogs != "7d" {
		t.Fatalf("created = %+v", created)
	}

	// Fields not given keep their values.
	var updated projectSettings
	server.decode("update_project", map[string]any{"project": "worker", "color": "#123456"}, &updated)

	if updated.Color != "#123456" || updated.RetentionLogs != "7d" || !slices.Equal(updated.EventLevels, []string{"error"}) || updated.Name != "worker" {
		t.Fatalf("updated = %+v", updated)
	}

	// An empty string returns a setting to the default.
	var reset projectSettings
	server.decode("update_project", map[string]any{"project": "worker", "retention_logs": ""}, &reset)

	if reset.RetentionLogs != "" || reset.Color != "#123456" {
		t.Fatalf("after reset = %+v, want the default retention and the color kept", reset)
	}

	if text, isError := server.try("create_project", map[string]any{"name": "Not Valid"}); !isError || !strings.Contains(text, "name:") {
		t.Fatalf("invalid name: %s", text)
	}

	if text, isError := server.try("create_project", map[string]any{"name": "backend"}); !isError || !strings.Contains(text, "Another project") {
		t.Fatalf("duplicate name: %s", text)
	}

	if text, isError := server.try("update_project", map[string]any{"project": "worker", "event_levels": []string{"error"}, "retention_events": "soon"}); !isError || !strings.Contains(text, "retention_events") {
		t.Fatalf("invalid retention: %s", text)
	}

	server.call("delete_project", map[string]any{"project": "worker"})

	if text := server.call("list_projects", nil); strings.Contains(text, "worker") {
		t.Fatalf("deleted project listed: %s", text)
	}

	if text, isError := server.try("delete_project", map[string]any{"project": "backend"}); !isError || !strings.Contains(text, "PROJECTS") {
		t.Fatalf("deleting a project from env: %s", text)
	}
}

type userOutput struct {
	ID                 int64  `json:"id"`
	Username           string `json:"username"`
	Name               string `json:"name"`
	Role               string `json:"role"`
	MustChangePassword bool   `json:"must_change_password"`
}

func TestManageUsers(t *testing.T) {
	server := newTestServer(t)

	var me userOutput
	server.decode("get_account", nil, &me)

	if me.Username != "admin" || me.Role != store.RoleSuperadmin {
		t.Fatalf("account = %+v", me)
	}

	var created struct {
		User              userOutput `json:"user"`
		TemporaryPassword string     `json:"temporary_password"`
	}
	server.decode("create_user", map[string]any{"username": "dev", "name": "Developer", "role": "user"}, &created)

	if created.TemporaryPassword == "" || !created.User.MustChangePassword || created.User.Role != store.RoleUser {
		t.Fatalf("created = %+v", created)
	}

	var updated userOutput
	server.decode("update_user", map[string]any{"user_id": created.User.ID, "role": "admin"}, &updated)

	if updated.Role != store.RoleAdmin || updated.Name != "Developer" {
		t.Fatalf("updated = %+v", updated)
	}

	// Nobody changes the superadmin or themselves here; it looks as if they did not exist.
	if text, isError := server.try("update_user", map[string]any{"user_id": me.ID, "role": "user"}); !isError || text != "not found" {
		t.Fatalf("update self: %s", text)
	}

	if text, isError := server.try("create_user", map[string]any{"username": "boss", "role": "superadmin"}); !isError {
		t.Fatalf("second superadmin: %s", text)
	}

	var reset struct {
		TemporaryPassword string `json:"temporary_password"`
	}
	server.decode("reset_user_password", map[string]any{"user_id": created.User.ID}, &reset)

	if reset.TemporaryPassword == "" || reset.TemporaryPassword == created.TemporaryPassword {
		t.Fatalf("reset = %+v", reset)
	}

	server.call("delete_user", map[string]any{"user_id": created.User.ID})

	if text := server.call("list_users", nil); strings.Contains(text, `"dev"`) {
		t.Fatalf("deleted user listed: %s", text)
	}

	server.decode("update_account_name", map[string]any{"name": "Root"}, &me)

	if me.Name != "Root" {
		t.Fatalf("account name = %q", me.Name)
	}
}

func TestManageConnections(t *testing.T) {
	server := newTestServer(t)

	// No SECRET_KEY on this server: chats cannot be added.
	if text, isError := server.try("create_connection", map[string]any{"type": "slack", "webhook_url": "https://hooks.slack.com/services/x"}); !isError || !strings.Contains(text, "not available") {
		t.Fatalf("slack without SECRET_KEY: %s", text)
	}

	// An address must be confirmed by its owner first.
	if text, isError := server.try("create_connection", map[string]any{"type": "email", "email": "dev@example.com"}); !isError || !strings.Contains(text, "start_email_verification") {
		t.Fatalf("unconfirmed email: %s", text)
	}

	if text := server.call("list_connections", nil); !strings.Contains(text, `"items":[]`) {
		t.Fatalf("connections: %s", text)
	}
}

func TestDeleteIssue(t *testing.T) {
	server := newTestServer(t)

	var issues issuePage
	server.decode("list_issues", map[string]any{"limit": 1}, &issues)

	id := issues.Items[0].ID
	server.call("delete_issue", map[string]any{"issue_id": id})

	if text, isError := server.try("get_issue", map[string]any{"issue_id": id}); !isError || text != "not found" {
		t.Fatalf("deleted issue: %s", text)
	}
}
