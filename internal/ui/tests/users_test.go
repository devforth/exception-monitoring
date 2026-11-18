package tests

import (
	"context"
	"html"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/ingest/grouping"
	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

var (
	temporaryPasswordField = regexp.MustCompile(`id="temporary-password" value="([^"]+)"`)
	userAction             = regexp.MustCompile(`action="/users/(\d+)"`)
)

const userPassword = "user password 123"

// createUser adds a user from the users page and lets them choose their
// password, as they must at the first login. It returns their id and a
// client logged in as them.
func (server *testServer) createUser(username, role string) (string, *testServer) {
	server.t.Helper()

	response := server.post("/users", url.Values{"username": {username}, "role": {role}})
	page := response.Body.String()
	temporary := temporaryPasswordField.FindStringSubmatch(page)
	id := userAction.FindStringSubmatch(page)

	if response.Code != http.StatusOK || temporary == nil || id == nil {
		server.t.Fatalf("create %s: %d %s", username, response.Code, page)
	}

	user := server.as(username, temporary[1])

	// A temporary password leads to the account page, and only there.
	if response := user.get("/issues"); response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/account" {
		server.t.Fatalf("temporary password: %d %q", response.Code, response.Header().Get("Location"))
	}

	response = user.post("/account/password", url.Values{"current_password": {temporary[1]}, "password": {userPassword}, "confirm_password": {userPassword}})

	if response.Code != http.StatusSeeOther {
		server.t.Fatalf("choose a password: %d %s", response.Code, response.Body)
	}

	user.page("/issues")

	return id[1], user
}

// What each role may open: users get issues, logs, the projects (read-only)
// and MCP; admins also the settings.
func TestRoles(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	_, admin := server.createUser("ops", store.RoleAdmin)
	userID, user := server.createUser("dev", store.RoleUser)

	for _, path := range []string{"/issues", "/logs", "/projects", "/agents", "/account"} {
		user.page(path)
	}

	for _, path := range []string{"/notifications", "/notifications/new", "/notifications/log", "/projects/new", "/projects/1", "/users", "/users/" + userID} {
		if response := user.get(path); response.Code != http.StatusNotFound {
			t.Errorf("user GET %s: %d", path, response.Code)
		}

		admin.page(path)
	}

	// The menu and the projects page show only what the role allows.
	menu := user.page("/projects")

	for _, hidden := range []string{`href="/notifications"`, `href="/users"`, `href="/projects/new"`, `href="/projects/1"`, "/delete"} {
		if strings.Contains(menu, hidden) {
			t.Errorf("a user sees %s", hidden)
		}
	}

	if !strings.Contains(admin.page("/projects"), `href="/projects/new"`) {
		t.Error("an admin cannot add projects")
	}

	if response := user.post("/projects", url.Values{"name": {"sneaky"}}); response.Code != http.StatusNotFound {
		t.Errorf("a user created a project: %d", response.Code)
	}
}

// Users change statuses but cannot delete issues: no button, and the server
// refuses it from the issue page and the list.
func TestUsersCannotDeleteIssues(t *testing.T) {
	server := newTestServer(t, password)
	server.ingestFixtures()
	server.login()

	_, user := server.createUser("dev", store.RoleUser)
	id := issueIDs(user.page("/issues"))[0]

	for _, page := range []string{user.page("/issues"), user.page("/issues/" + id)} {
		if strings.Contains(page, `value="delete"`) {
			t.Error("a user sees the Delete button")
		}
	}

	if !strings.Contains(server.page("/issues/"+id), `value="delete"`) {
		t.Error("the superadmin has no Delete button")
	}

	if response := user.post("/issues/"+id+"/action", url.Values{"action": {"delete"}}); response.Code != http.StatusNotFound {
		t.Errorf("delete from the issue page: %d", response.Code)
	}

	if response := user.post("/issues/bulk", url.Values{"action": {"delete"}, "issue": {id}}); response.Code != http.StatusNotFound {
		t.Errorf("bulk delete: %d", response.Code)
	}

	user.page("/issues/" + id)

	if response := user.post("/issues/"+id+"/action", url.Values{"action": {"resolve"}}); response.Code != http.StatusSeeOther {
		t.Errorf("a user cannot resolve: %d", response.Code)
	}
}

// Admins manage admins and users, never the superadmin or themselves here.
func TestAdminsManageUsers(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	otherID, _ := server.createUser("ops2", store.RoleAdmin)
	_, admin := server.createUser("ops", store.RoleAdmin)

	// The admin edits the other admin, not the superadmin, whose page is read-only.
	if users := admin.page("/users"); !strings.Contains(users, `href="/users/`+otherID+`" title="Edit`) || strings.Contains(users, `href="/users/1" title="Edit`) {
		t.Errorf("edit links:\n%s", users)
	}

	if page := admin.page("/users/1"); strings.Contains(page, `action="/users/1"`) || !strings.Contains(page, "cannot change this user") {
		t.Errorf("superadmin page for an admin:\n%s", page)
	}

	for _, request := range []struct {
		path string
		form url.Values
	}{
		{"/users/1", url.Values{"name": {"Boss"}, "role": {"user"}}},
		{"/users/1/password", nil},
		{"/users/1/delete", nil},
		{"/users", url.Values{"username": {"second"}, "role": {"superadmin"}}},
	} {
		response := admin.post(request.path, request.form)

		if response.Code != http.StatusNotFound && response.Code != http.StatusUnprocessableEntity {
			t.Errorf("admin POST %s: %d", request.path, response.Code)
		}
	}

	if response := admin.post("/users/"+otherID, url.Values{"name": {"Second admin"}, "role": {"user"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("admin edits an admin: %d %s", response.Code, response.Body)
	}

	if page := admin.page("/users"); !strings.Contains(page, "Second admin") {
		t.Error("the change is not shown")
	}

	if response := admin.post("/users/"+otherID+"/delete", nil); response.Code != http.StatusSeeOther {
		t.Fatalf("admin deletes a user: %d", response.Code)
	}

	if response := server.post("/users", url.Values{"username": {"OPS"}, "role": {"user"}}); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "taken") {
		t.Errorf("a username in another case: %d", response.Code)
	}
}

// A password reset by someone else ends the user's sessions and disconnects
// their agents; changing your own keeps your agents and this session.
func TestPasswordChangesAndAgents(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	id, user := server.createUser("dev", store.RoleUser)
	token := personalToken(t, user)
	other := server.as("dev", userPassword)

	// Own change: this session stays, the other ends, the agent works.
	response := user.post("/account/password", url.Values{"current_password": {"wrong"}, "password": {"new password 456"}, "confirm_password": {"new password 456"}})

	if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "current password is wrong") {
		t.Fatalf("wrong current password: %d", response.Code)
	}

	response = user.post("/account/password", url.Values{"current_password": {userPassword}, "password": {"new password 456"}, "confirm_password": {"new password 456"}})

	if response.Code != http.StatusSeeOther {
		t.Fatalf("own change: %d %s", response.Code, response.Body)
	}

	user.page("/issues")

	if response := other.get("/issues"); response.Code != http.StatusSeeOther {
		t.Error("the other session survived the password change")
	}

	agentAccess(t, server, token, http.StatusOK)

	// A reset by an admin: logged out, agents disconnected, a temporary password.
	page := server.post("/users/"+id+"/password", nil).Body.String()

	if temporaryPasswordField.FindStringSubmatch(page) == nil {
		t.Fatalf("no temporary password:\n%s", page)
	}

	if response := user.get("/issues"); response.Code != http.StatusSeeOther {
		t.Error("the session survived a reset")
	}

	agentAccess(t, server, token, http.StatusUnauthorized)

	// Deleting a user disconnects their agents too.
	deletedID, deleted := server.createUser("gone", store.RoleUser)
	token = personalToken(t, deleted)

	if response := server.post("/users/"+deletedID+"/delete", nil); response.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", response.Code)
	}

	agentAccess(t, server, token, http.StatusUnauthorized)
}

// personalToken generates the personal token of the client's user.
func personalToken(t *testing.T, client *testServer) string {
	t.Helper()

	match := personalTokenField.FindStringSubmatch(client.post("/agents/token", nil).Body.String())

	if match == nil {
		t.Fatal("no personal token")
	}

	return match[1]
}

// Admins list the connected agents of any user; users only their own.
func TestAgentsOfOtherUsers(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	id, user := server.createUser("dev", store.RoleUser)
	token := personalToken(t, user)
	agentAccess(t, server, token, http.StatusOK)

	if page := server.page("/agents"); strings.Contains(page, "/disconnect") || !strings.Contains(page, `<select name="user"`) {
		t.Error("the superadmin's own list shows another user's agent, or no user select")
	}

	page := server.page("/agents?user=" + id)
	disconnect := disconnectAction.FindStringSubmatch(page)

	if disconnect == nil {
		t.Fatalf("the user's agent is not listed:\n%s", page)
	}

	if strings.Contains(user.page("/agents"), `<select name="user"`) {
		t.Error("a user can choose whose agents to list")
	}

	if response := user.get("/agents?user=1"); !strings.Contains(response.Body.String(), disconnect[1]) {
		t.Error("?user= changed what a user sees")
	}

	ownerOnly := server.as("admin", password)
	ownToken := personalToken(t, ownerOnly)
	agentAccess(t, server, ownToken, http.StatusOK)

	ownGrant := disconnectAction.FindStringSubmatch(server.page("/agents"))

	if response := user.post(ownGrant[1], nil); response.Code != http.StatusNotFound {
		t.Errorf("a user disconnected the superadmin's agent: %d", response.Code)
	}

	if response := server.post(disconnect[1], url.Values{"user": {id}}); response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/agents?client=claude&user="+id {
		t.Fatalf("disconnect another user's agent: %d %q", response.Code, response.Header().Get("Location"))
	}

	agentAccess(t, server, token, http.StatusUnauthorized)
}

// Every status change is in the issue's history: who, when and how; a
// resolved issue that happens again is reopened by its event.
func TestStatusHistory(t *testing.T) {
	database := openDatabase(t)
	server := startServer(t, database, "admin", password)
	server.ingestFixtures()
	server.login()

	_, user := server.createUser("dev", store.RoleUser)
	id := issueIDs(server.page("/issues"))[0]

	// The name field starts with the current name: the username until one is set.
	if page := user.page("/account"); !strings.Contains(page, `<input name="name" value="dev"`) {
		t.Error("the name field is empty")
	}

	user.post("/account/name", url.Values{"name": {"Dev One"}})

	if page := user.page("/account"); !strings.Contains(page, `<input name="name" value="Dev One"`) {
		t.Error("the saved name is not shown")
	}

	// An empty name is saved as the username.
	server.post("/account/name", url.Values{"name": {" "}})

	if page := server.page("/account"); !strings.Contains(page, `<input name="name" value="admin"`) {
		t.Error("an empty name is not the username")
	}

	user.post("/issues/"+id+"/action", url.Values{"action": {"resolve"}})
	server.post("/issues/bulk", url.Values{"action": {"ignore"}, "issue": {id}})
	server.post("/issues/"+id+"/action", url.Values{"action": {"ignore"}}) // no change, no entry

	page := html.UnescapeString(server.page("/issues/" + id))
	history := page[strings.Index(page, "<h2>Status history</h2>"):]
	history = history[:strings.Index(history, "</section>")]

	if strings.Count(history, "<li>") != 2 || !strings.Contains(history, "by admin") || !strings.Contains(history, "by Dev One") ||
		strings.Index(history, "by admin") > strings.Index(history, "by Dev One") {
		t.Fatalf("history:\n%s", history)
	}

	if !strings.Contains(page, `class="status status-ignored"`) || !strings.Contains(page, "by admin ·") {
		t.Error("the header does not say who changed the status")
	}

	// A resolved issue that happens again: reopened by its event.
	issueID, _ := strconv.ParseInt(id, 10, 64)
	ctx := context.Background()

	if err := database.SetIssueStatus(ctx, issueID, store.StatusResolved, store.Actor{Name: "API token", Source: store.SourceAPI}); err != nil {
		t.Fatal(err)
	}

	issue, err := database.GetIssue(ctx, issueID)

	if err != nil {
		t.Fatal(err)
	}

	// The fingerprint of the issue, from its stored event as ingest computes it.
	latest, err := database.LatestEvent(ctx, issueID)

	if err != nil {
		t.Fatal(err)
	}

	parsed, err := sentry.ParseEvent(latest.Payload, latest.EventID, time.Now())

	if err != nil {
		t.Fatal(err)
	}

	fingerprint := grouping.Fingerprint(parsed)

	again := store.NewEvent{
		ProjectID: issue.ProjectID, EventID: "0123456789abcdef0123456789abcdef", Fingerprint: fingerprint,
		Title: issue.Title, Level: issue.Level, Timestamp: time.Now(), ReceivedAt: time.Now(), Payload: []byte(`{}`),
	}

	if result, err := database.SaveEvent(ctx, again); err != nil || !result.Regression {
		t.Fatal("not a regression", result, err)
	}

	page = html.UnescapeString(server.page("/issues/" + id))

	if !strings.Contains(page, "reopened: happened again") || !strings.Contains(page, "by API token") {
		t.Errorf("reopening not in the history:\n%s", page)
	}
}

// A stolen session must not become a way to guess the password: wrong
// current passwords count against the login limit.
func TestAccountPasswordGuessing(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	wrong := url.Values{"current_password": {"guess guess guess"}, "password": {"new password 456"}, "confirm_password": {"new password 456"}}

	for range 10 {
		server.post("/account/password", wrong)
	}

	right := url.Values{"current_password": {password}, "password": {"new password 456"}, "confirm_password": {"new password 456"}}

	if response := server.post("/account/password", right); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "Too many wrong passwords") {
		t.Fatalf("after 10 guesses: %d", response.Code)
	}
}

// Admins manage the agents of users and admins, not the superadmin's.
func TestAdminsCannotTouchSuperadminAgents(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	agentAccess(t, server, personalToken(t, server), http.StatusOK)

	_, admin := server.createUser("ops", store.RoleAdmin)
	superadminGrant := disconnectAction.FindStringSubmatch(server.page("/agents"))

	if page := admin.page("/agents?user=1"); strings.Contains(page, superadminGrant[1]) || strings.Contains(page, `<option value="1"`) {
		t.Errorf("an admin sees the superadmin's agents:\n%s", page)
	}

	if response := admin.post(superadminGrant[1], nil); response.Code != http.StatusNotFound {
		t.Errorf("an admin disconnected the superadmin's agent: %d", response.Code)
	}
}
