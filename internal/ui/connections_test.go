package ui_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/fixtures"
	"github.com/devforth/exception-monitoring/internal/ui"
)

// webhook is a fake Slack incoming webhook that keeps the posted bodies.
type webhook struct {
	mutex  sync.Mutex
	bodies []string
}

func newWebhook(t *testing.T) (*webhook, string) {
	t.Helper()

	hook := &webhook{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)

		hook.mutex.Lock()
		hook.bodies = append(hook.bodies, string(body))
		hook.mutex.Unlock()
	}))
	t.Cleanup(server.Close)

	return hook, server.URL + "/services/T0/B0/x"
}

func (hook *webhook) count() int {
	hook.mutex.Lock()
	defer hook.mutex.Unlock()

	return len(hook.bodies)
}

func (hook *webhook) last() string {
	hook.mutex.Lock()
	defer hook.mutex.Unlock()

	if len(hook.bodies) == 0 {
		return ""
	}

	return hook.bodies[len(hook.bodies)-1]
}

var (
	connectionLink   = regexp.MustCompile(`class="issue-title" href="/notifications/(\d+)"`)
	destinationInput = regexp.MustCompile(`name="destination_id" value="(\d+)"`)
	confirmationCode = regexp.MustCompile(`\b(\d{6})\b`)
)

func TestSlackConnection(t *testing.T) {
	database := openDatabase(t)
	server := startServer(t, database, "admin", password)
	server.login()

	if body := server.page("/notifications"); !strings.Contains(body, "No connections yet") || !strings.Contains(body, `href="/notifications/new"`) {
		t.Fatal("empty notifications page")
	}

	hook, webhookURL := newWebhook(t)

	created := server.post("/notifications", url.Values{
		"name": {"Team chat"}, "type": {"slack"}, "webhook_url": {webhookURL},
		"all_projects": {"on"}, "level": {"error", "fatal"}, "active": {"on"},
	})

	if created.Code != http.StatusSeeOther || created.Header().Get("Location") != "/notifications?notice=created" {
		t.Fatalf("create: %d %q\n%s", created.Code, created.Header().Get("Location"), created.Body)
	}

	list := server.page("/notifications")

	for _, want := range []string{"Team chat", "Slack", "All projects", "error, fatal", "connection-active"} {
		if !strings.Contains(list, want) {
			t.Errorf("list is missing %q", want)
		}
	}

	// The webhook URL is stored encrypted, in the destination.
	var stored string

	if err := database.DB.QueryRow(`SELECT secret FROM notification_destinations WHERE type = 'slack'`).Scan(&stored); err != nil {
		t.Fatal(err)
	}

	if stored == "" || strings.Contains(stored, "hooks") || strings.Contains(stored, "services") {
		t.Fatalf("the webhook URL is not stored encrypted: %q", stored)
	}

	id := connectionLink.FindStringSubmatch(list)[1]

	if response := server.post("/notifications/"+id+"/test", nil); response.Code != http.StatusSeeOther || !strings.Contains(hook.last(), "Test message") {
		t.Fatalf("test message: %d, webhook got %q", response.Code, hook.last())
	}

	// Editing without a new URL keeps the stored one.
	edited := server.post("/notifications/"+id, url.Values{"name": {"Backend chat"}, "project": {"1"}, "level": {"warning"}, "active": {"on"}})

	if edited.Code != http.StatusSeeOther {
		t.Fatalf("edit: %d\n%s", edited.Code, edited.Body)
	}

	list = server.page("/notifications")

	if !strings.Contains(list, "Backend chat") || !strings.Contains(list, `data-project="backend"`) || !strings.Contains(list, "<td>warning</td>") {
		t.Fatalf("after edit:\n%s", list)
	}

	if server.post("/notifications/"+id+"/test", nil).Code != http.StatusSeeOther || hook.count() != 2 {
		t.Fatal("the kept webhook URL must still work")
	}

	for _, state := range []struct {
		value  string
		status string
		button string
		notice string
	}{
		{"0", "inactive", "Activate", "deactivated"},
		{"1", "active", "Deactivate", "activated"},
	} {
		response := server.post("/notifications/"+id+"/active", url.Values{"active": {state.value}})

		if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/notifications?notice="+state.notice {
			t.Fatalf("set active %s: %d\n%s", state.value, response.Code, response.Body)
		}

		list = server.page("/notifications")

		for _, want := range []string{"Backend chat", `data-project="backend"`, "<td>warning</td>", "connection-" + state.status, `aria-label="` + state.button + ` Backend chat"`, `aria-label="Edit Backend chat"`, `action="/notifications/` + id + `/delete"`, "data-confirm="} {
			if !strings.Contains(list, want) {
				t.Errorf("after setting active %s, missing %q", state.value, want)
			}
		}
	}

	if response := server.post("/notifications/"+id+"/active", url.Values{"active": {"invalid"}}); response.Code != http.StatusBadRequest {
		t.Fatalf("invalid active value: %d", response.Code)
	}

	// One connection per webhook: the error links to the one that has it.
	link := `href="/notifications/` + id + `"`
	same := url.Values{"name": {"Again"}, "type": {"slack"}, "webhook_url": {webhookURL}, "all_projects": {"on"}, "level": {"error"}}

	if response := server.post("/notifications", same); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "Already added as <a "+link) {
		t.Fatalf("same webhook again: %d\n%s", response.Code, response.Body)
	}

	// Saving a connection with its own webhook is fine; moving another one onto it is not.
	if response := server.post("/notifications/"+id, url.Values{"name": {"Backend chat"}, "webhook_url": {webhookURL}, "project": {"1"}, "level": {"warning"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("own webhook: %d\n%s", response.Code, response.Body)
	}

	_, otherURL := newWebhook(t)
	same.Set("webhook_url", otherURL)

	if response := server.post("/notifications", same); response.Code != http.StatusSeeOther {
		t.Fatalf("another webhook: %d\n%s", response.Code, response.Body)
	}

	otherID := connectionLink.FindAllStringSubmatch(server.page("/notifications"), -1)[1][1]

	same.Set("webhook_url", webhookURL)

	if response := server.post("/notifications/"+otherID, same); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), link) {
		t.Fatalf("moving onto a used webhook: %d\n%s", response.Code, response.Body)
	}

	server.post("/notifications/"+otherID+"/delete", nil)

	if response := server.post("/notifications/"+id+"/delete", nil); response.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", response.Code)
	}

	if !strings.Contains(server.page("/notifications"), "No connections yet") {
		t.Fatal("deleted connection still listed")
	}
}

// receiveCode waits for the next mail and returns its code and recipient commands.
func receiveCode(t *testing.T, server *testServer) (code string, commands string) {
	t.Helper()

	select {
	case mail := <-server.mails:
		_, text, _ := fixtures.ParseEmail(t, mail.Data)

		return confirmationCode.FindString(text), strings.Join(mail.Commands, "\n")
	case <-time.After(5 * time.Second):
		t.Fatal("no confirmation email")

		return "", ""
	}
}

func expectNoMail(t *testing.T, server *testServer, why string) {
	t.Helper()

	select {
	case <-server.mails:
		t.Fatal(why)
	case <-time.After(100 * time.Millisecond):
	}
}

func TestEmailStep(t *testing.T) {
	database := openDatabase(t)
	server := startServer(t, database, "admin", password)
	server.login()

	// The new form starts locked on the email step.
	if form := server.page("/notifications/new"); !strings.Contains(form, `hx-post="/notifications/email/start"`) || !strings.Contains(form, `class="connection-rest" disabled`) {
		t.Fatal("a new email connection must start with the address and the rest locked")
	}

	if step := server.post("/notifications/email/start", url.Values{"address": {"not an address"}}).Body.String(); !strings.Contains(step, "valid email address") {
		t.Fatalf("invalid address:\n%s", step)
	}

	// Mixed case in, lowercase stored and mailed.
	step := server.post("/notifications/email/start", url.Values{"address": {"Ops@Example.COM"}}).Body.String()

	if !strings.Contains(step, `name="code"`) || !strings.Contains(step, "Send the code again") || !strings.Contains(step, "Change email") {
		t.Fatalf("after Verify:\n%s", step)
	}

	destinationID := destinationInput.FindStringSubmatch(step)[1]
	code, commands := receiveCode(t, server)

	if !strings.Contains(commands, "RCPT TO:<ops@example.com>") {
		t.Fatalf("code sent to %s", commands)
	}

	var stored string

	if err := database.DB.QueryRow(`SELECT address FROM notification_destinations WHERE id = ?`, destinationID).Scan(&stored); err != nil || stored != "ops@example.com" {
		t.Fatalf("stored address %q (%v), want lowercase", stored, err)
	}

	connection := url.Values{
		"type": {"email"}, "destination_id": {destinationID}, "name": {"Ops"},
		"all_projects": {"on"}, "level": {"error"}, "active": {"on"},
	}

	// Saving before the code is confirmed is refused.
	if response := server.post("/notifications", connection); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "Confirm the email address first") {
		t.Fatalf("save unconfirmed: %d", response.Code)
	}

	wrongCode := "000000"

	if code == wrongCode {
		wrongCode = "111111"
	}

	if step := server.post("/notifications/email/confirm", url.Values{"destination_id": {destinationID}, "code": {wrongCode}}).Body.String(); !strings.Contains(step, "Wrong code") {
		t.Fatalf("wrong code:\n%s", step)
	}

	step = server.post("/notifications/email/confirm", url.Values{"destination_id": {destinationID}, "code": {code}}).Body.String()

	if !strings.Contains(step, "data-verified") || !strings.Contains(step, "✓ confirmed") {
		t.Fatalf("right code:\n%s", step)
	}

	if response := server.post("/notifications", connection); response.Code != http.StatusSeeOther {
		t.Fatalf("save confirmed: %d\n%s", response.Code, response.Body)
	}

	list := server.page("/notifications")

	if !strings.Contains(list, "ops@example.com") || !strings.Contains(list, "connection-active") {
		t.Fatalf("list:\n%s", list)
	}

	id := connectionLink.FindStringSubmatch(list)[1]

	for _, include := range []string{"on", ""} {
		form := url.Values{"name": {"Ops"}, "all_projects": {"on"}, "level": {"error"}, "active": {"on"}, "include_issue_link": {include}}

		if response := server.post("/notifications/"+id, form); response.Code != http.StatusSeeOther {
			t.Fatalf("save link option: %d", response.Code)
		}

		var hidden bool

		if err := database.DB.QueryRow(`SELECT hide_issue_link FROM notification_connections WHERE id = ?`, id).Scan(&hidden); err != nil || hidden != (include == "") {
			t.Fatalf("saved link option: hidden=%v, err=%v", hidden, err)
		}

		page := server.page("/notifications/" + id)

		if strings.Contains(page, `name="include_issue_link" checked`) != (include == "on") {
			t.Fatal("link checkbox must reflect saved setting")
		}
	}

	// One connection per address: no code, a link to the existing one.
	step = server.post("/notifications/email/start", url.Values{"address": {"OPS@example.com"}}).Body.String()

	if !strings.Contains(step, `Already added as <a href="/notifications/`+id+`"`) {
		t.Fatalf("an address already added:\n%s", step)
	}

	expectNoMail(t, server, "an address already added must not get a code")

	if response := server.post("/notifications", connection); response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), "Already added") {
		t.Fatalf("second connection to the same address: %d", response.Code)
	}

	// Turning it off and on needs no new code.
	for _, active := range []string{"", "on"} {
		form := url.Values{"name": {"Ops"}, "all_projects": {"on"}, "level": {"error"}}

		if active != "" {
			form.Set("active", active)
		}

		if response := server.post("/notifications/"+id, form); response.Code != http.StatusSeeOther {
			t.Fatalf("toggle active=%q: %d\n%s", active, response.Code, response.Body)
		}
	}

	// After deleting the connection the address stays confirmed: no new code.
	if response := server.post("/notifications/"+id+"/delete", nil); response.Code != http.StatusSeeOther {
		t.Fatalf("delete: %d", response.Code)
	}

	step = server.post("/notifications/email/start", url.Values{"address": {"OPS@example.com"}}).Body.String()

	if !strings.Contains(step, "data-verified") || !strings.Contains(step, "Confirmed earlier") || destinationInput.FindStringSubmatch(step)[1] != destinationID {
		t.Fatalf("a confirmed address again:\n%s", step)
	}

	expectNoMail(t, server, "a confirmed address must not get another code")

	// "Change email" goes back to the address field.
	if step := server.post("/notifications/email/change", url.Values{"address": {"ops@example.com"}}).Body.String(); !strings.Contains(step, `hx-post="/notifications/email/start"`) {
		t.Fatalf("change email:\n%s", step)
	}
}

// A connection whose address was never confirmed (made before confirmation
// existed) is confirmed on its own page; the address itself cannot change.
func TestConfirmExistingConnection(t *testing.T) {
	database := openDatabase(t)
	server := startServer(t, database, "admin", password)
	server.login()

	_, err := database.DB.Exec(`
		INSERT INTO notification_destinations (id, type, address, secret, verified, created_at, updated_at) VALUES (7, 'email', 'old@example.com', '', 0, 'x', 'x');
		INSERT INTO notification_connections (id, name, destination_id, all_projects, levels, active, created_at, updated_at) VALUES (3, 'Old', 7, 1, 'error', 0, 'x', 'x');`)

	if err != nil {
		t.Fatal(err)
	}

	page := server.page("/notifications/3")

	if list := server.page("/notifications"); !strings.Contains(list, `disabled title="Confirm the email address first"`) {
		t.Fatal("activation must be disabled until email confirmation")
	}

	if response := server.post("/notifications/3/active", url.Values{"active": {"1"}}); response.Code != http.StatusUnprocessableEntity {
		t.Fatalf("activate unconfirmed email: %d", response.Code)
	}

	var active bool

	if err := database.DB.QueryRow(`SELECT active FROM notification_connections WHERE id = 3`).Scan(&active); err != nil || active {
		t.Fatalf("unconfirmed connection became active: %v, %v", active, err)
	}

	if !strings.Contains(page, `hx-post="/notifications/email/resend"`) || !strings.Contains(page, `class="connection-rest" disabled`) || strings.Contains(page, "Change email") {
		t.Fatalf("an unconfirmed connection must offer a code, lock the rest and keep the address:\n%s", page)
	}

	form := url.Values{"address_fixed": {"1"}, "destination_id": {"7"}}
	step := server.post("/notifications/email/resend", form).Body.String()

	if !strings.Contains(step, `name="code"`) || strings.Contains(step, "Change email") {
		t.Fatalf("after Send a code:\n%s", step)
	}

	code, _ := receiveCode(t, server)
	form.Set("code", code)

	if step := server.post("/notifications/email/confirm", form).Body.String(); !strings.Contains(step, "data-verified") {
		t.Fatalf("confirm:\n%s", step)
	}

	if response := server.post("/notifications/3", url.Values{"name": {"Old"}, "all_projects": {"on"}, "level": {"error"}, "active": {"on"}}); response.Code != http.StatusSeeOther {
		t.Fatalf("activate after confirming: %d\n%s", response.Code, response.Body)
	}

	if list := server.page("/notifications"); !strings.Contains(list, "connection-active") {
		t.Fatalf("list:\n%s", list)
	}
}

func TestResendCooldown(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	step := server.post("/notifications/email/start", url.Values{"address": {"ops@example.com"}}).Body.String()
	destinationID := destinationInput.FindStringSubmatch(step)[1]
	oldCode, _ := receiveCode(t, server)

	// Right away: refused, so the button cannot flood a mailbox.
	if step := server.post("/notifications/email/resend", url.Values{"destination_id": {destinationID}}).Body.String(); !strings.Contains(step, "send again in") {
		t.Fatalf("immediate resend:\n%s", step)
	}

	expectNoMail(t, server, "a resend within the cooldown must not send mail")

	restore := ui.SetResendCooldown(0)

	defer restore()

	server.post("/notifications/email/resend", url.Values{"destination_id": {destinationID}})
	newCode, _ := receiveCode(t, server)

	// Only the newest code works.
	if oldCode != newCode {
		if step := server.post("/notifications/email/confirm", url.Values{"destination_id": {destinationID}, "code": {oldCode}}).Body.String(); !strings.Contains(step, "Wrong code") {
			t.Fatalf("old code accepted:\n%s", step)
		}
	}

	if step := server.post("/notifications/email/confirm", url.Values{"destination_id": {destinationID}, "code": {newCode}}).Body.String(); !strings.Contains(step, "data-verified") {
		t.Fatalf("new code:\n%s", step)
	}
}

func TestConnectionFormValidation(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	testCases := []struct {
		name string
		form url.Values
		want string
	}{
		{"name too long", url.Values{"name": {strings.Repeat("x", 101)}, "type": {"slack"}, "webhook_url": {"https://hooks.slack.com/x"}, "all_projects": {"on"}, "level": {"error"}}, "Enter a name"},
		{"email without confirmation", url.Values{"name": {"x"}, "type": {"email"}, "destination_id": {"999"}, "all_projects": {"on"}, "level": {"error"}}, "Confirm the email address first"},
		{"bad telegram token", url.Values{"name": {"x"}, "type": {"telegram"}, "bot_token": {"nope"}, "chat_id": {"42"}, "all_projects": {"on"}, "level": {"error"}}, "bot token"},
		{"bad slack url", url.Values{"name": {"x"}, "type": {"slack"}, "webhook_url": {"hooks.slack.com"}, "all_projects": {"on"}, "level": {"error"}}, "webhook URL"},
		{"no projects", url.Values{"name": {"x"}, "type": {"slack"}, "webhook_url": {"https://hooks.slack.com/x"}, "level": {"error"}}, "at least one project"},
		{"no levels", url.Values{"name": {"x"}, "type": {"slack"}, "webhook_url": {"https://hooks.slack.com/x"}, "all_projects": {"on"}}, "at least one level"},
		{"unknown type", url.Values{"name": {"x"}, "type": {"discord"}, "all_projects": {"on"}, "level": {"error"}}, "not available"},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			response := server.post("/notifications", testCase.form)

			if response.Code != http.StatusUnprocessableEntity || !strings.Contains(response.Body.String(), testCase.want) {
				t.Fatalf("got %d, want 422 with %q", response.Code, testCase.want)
			}
		})
	}

	if connectionLink.MatchString(server.page("/notifications")) {
		t.Fatal("an invalid form must not create a connection")
	}
}

// A connection saved without a name gets one: the part of the address before
// "@", the name of the Telegram chat, or "Slack".
func TestConnectionDefaultNames(t *testing.T) {
	database := openDatabase(t)
	server := startServer(t, database, "admin", password)
	server.login()

	step := server.post("/notifications/email/start", url.Values{"address": {"Ops.Team@example.com"}}).Body.String()
	destinationID := destinationInput.FindStringSubmatch(step)[1]
	code, _ := receiveCode(t, server)
	server.post("/notifications/email/confirm", url.Values{"destination_id": {destinationID}, "code": {code}})

	_, webhookURL := newWebhook(t)

	for _, form := range []url.Values{
		{"type": {"email"}, "destination_id": {destinationID}},
		{"type": {"telegram"}, "bot_token": {"123456:ABCDEFGHIJKLMNOPQRSTUVWXYZ"}, "chat_id": {"-100"}},
		{"type": {"slack"}, "webhook_url": {webhookURL}},
	} {
		form.Set("name", "")
		form.Set("all_projects", "on")
		form.Set("level", "error")

		if response := server.post("/notifications", form); response.Code != http.StatusSeeOther {
			t.Fatalf("%s without a name: %d\n%s", form.Get("type"), response.Code, response.Body)
		}
	}

	var names []string

	rows, err := database.DB.Query(`SELECT name FROM notification_connections ORDER BY id`)

	if err != nil {
		t.Fatal(err)
	}

	defer rows.Close()

	for rows.Next() {
		var name string

		_ = rows.Scan(&name)
		names = append(names, name)
	}

	if strings.Join(names, "|") != "ops.team|Ops chat|Slack" {
		t.Fatalf("names: %q", names)
	}

	if page := server.page("/notifications/new"); !strings.Contains(page, `placeholder="team"`) || strings.Contains(page, `name="name" value="" maxlength="100" required`) {
		t.Error("the name must be optional, with a placeholder")
	}
}

// The delivery log shows every message with its status and error, and the
// notifications list flags a connection whose deliveries fail.
func TestDeliveryLog(t *testing.T) {
	database := openDatabase(t)
	server := startServer(t, database, "admin", password)
	server.login()

	for _, name := range []string{"Broken chat", "Working chat"} {
		_, webhookURL := newWebhook(t)
		form := url.Values{"name": {name}, "type": {"slack"}, "webhook_url": {webhookURL}, "all_projects": {"on"}, "level": {"error"}}

		if response := server.post("/notifications", form); response.Code != http.StatusSeeOther {
			t.Fatalf("create %s: %d", name, response.Code)
		}
	}

	ctx := context.Background()
	now := time.Now()
	notification := `{"Kind":"new_issue","Project":"backend","Title":"Error: queue is full","IssueID":7}`

	for _, channel := range []string{"1", "2"} {
		if err := database.EnqueueOutbox(ctx, []string{channel}, "notification", []byte(notification), nil, now); err != nil {
			t.Fatal(err)
		}
	}

	if err := database.MarkOutboxRetry(ctx, 1, "slack: 500 internal_error", now.Add(time.Minute), now); err != nil {
		t.Fatal(err)
	}

	if err := database.MarkOutboxSent(ctx, 2, now); err != nil {
		t.Fatal(err)
	}

	list := server.page("/notifications")

	if !strings.Contains(list, "failing: slack: 500 internal_error") || !strings.Contains(list, `href="/notifications/log?connection=2">ok`) {
		t.Fatalf("delivery column:\n%s", list)
	}

	log := server.page("/notifications/log")

	for _, want := range []string{"Broken chat", "Working chat", `href="/issues/7">Error: queue is full</a>`, "retrying", "slack: 500 internal_error", `delivery-sent`} {
		if !strings.Contains(log, want) {
			t.Errorf("delivery log is missing %q", want)
		}
	}

	if filtered := server.page("/notifications/log?status=sent&connection=999"); strings.Contains(filtered, "internal_error") || !strings.Contains(filtered, "Working chat") {
		t.Error("the status filter must apply and an unknown connection must be ignored")
	}
}
