package notify

import (
	"context"
	stdhtml "html"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/fixtures"
	"github.com/devforth/exception-monitoring/internal/ingest/sentry"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

func fixtureEvent(t *testing.T, name string) *sentry.Event {
	t.Helper()

	envelope, err := sentry.ParseEnvelope(fixtures.Envelope(t, name))

	if err != nil {
		t.Fatal(err)
	}

	event, err := sentry.ParseEvent(envelope.Items[0].Payload, envelope.Header.EventID, time.Now())

	if err != nil {
		t.Fatal(err)
	}

	return event
}

func TestEmail(t *testing.T) {
	settings, sessions := fixtures.FakeSMTP(t)
	event := fixtureEvent(t, "node-event-cause-chain")

	notification := Notification{
		Kind:         NewIssue,
		Project:      "backend",
		ProjectColor: "#db2777",
		IssueID:      "issue005",
		IssueURL:     "https://errors.example.com/issues/issue005",
		Title:        event.Title(),
		Culprit:      "GET /api/debug-sentry/cause",
		Level:        "error",
		Environment:  event.Environment,
		Time:         time.Date(2026, 11, 12, 10, 0, 0, 0, time.UTC),
		Event:        event,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)

	defer cancel()

	if err := (Email{SMTP: settings, To: "ops@example.com"}).Send(ctx, notification); err != nil {
		t.Fatal(err)
	}

	session := <-sessions
	commands := strings.Join(session.Commands, "\n")

	for _, want := range []string{"AUTH PLAIN", "MAIL FROM:<errors@example.com>", "RCPT TO:<ops@example.com>"} {
		if !strings.Contains(commands, want) {
			t.Errorf("SMTP session is missing %q:\n%s", want, commands)
		}
	}

	subject, text, html := fixtures.ParseEmail(t, session.Data)

	for _, body := range []string{text, stdhtml.UnescapeString(html)} {
		if !strings.Contains(body, `Exception Monitoring MCP: get_issue {"issue_id":"issue005"}`) {
			t.Error("missing copyable MCP reference in email body")
		}
	}

	if subject != "[backend] New issue: Error: Failed to load orders" {
		t.Errorf("subject = %q", subject)
	}

	// The agent prompt is attached, not in the body.
	prompt := fixtures.ReadEmail(t, session.Data).Attachments["backend-error-issue005.md"]

	for _, want := range []string{"## Exception", "Kind: New issue", "Do not change any code"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("attached prompt is missing %q", want)
		}
	}

	if strings.Contains(html, "## Exception") || strings.Contains(text, "## Exception") || !strings.Contains(html, "backend-error-issue005.md") || !strings.Contains(text, "backend-error-issue005.md") {
		t.Error("the body must mention the attached prompt, not contain it")
	}

	// The project is marked with its color.
	if !strings.Contains(html, "border:1px solid #db2777;") {
		t.Error("the project badge is missing")
	}

	// The long format: cause chain, source line of the throw, link, environment.
	for _, want := range []string{"Caused by", "connect ECONNREFUSED", `throw new Error(&#34;Failed to load orders&#34;`, `href="https://errors.example.com/issues/issue005"`, "development"} {
		if !strings.Contains(html, want) {
			t.Errorf("HTML part is missing %q", want)
		}
	}

	if !strings.Contains(text, "Caused by Error: connect ECONNREFUSED") || !strings.Contains(text, "https://errors.example.com/issues/issue005") {
		t.Errorf("text part:\n%s", text)
	}
}

func TestEmailSummary(t *testing.T) {
	settings, sessions := fixtures.FakeSMTP(t)

	summary := Summary{Count: 3, Window: time.Minute, Items: []Notification{{Project: "backend", IssueID: "issue042", Title: "Error: <b>boom</b>"}}}

	if err := (Email{SMTP: settings, To: "ops@example.com"}).SendSummary(context.Background(), summary); err != nil {
		t.Fatal(err)
	}

	subject, text, html := fixtures.ParseEmail(t, (<-sessions).Data)

	for _, body := range []string{text, stdhtml.UnescapeString(html)} {
		if !strings.Contains(body, `Exception Monitoring MCP: get_issue {"issue_id":"issue042"}`) {
			t.Error("missing MCP reference in summary")
		}
	}

	if subject != "[Exception Monitoring] 3 more issues in the last 1m0s" || strings.Contains(html, "<b>boom</b>") {
		t.Fatalf("subject %q; HTML must escape titles:\n%s", subject, html)
	}
}

func TestEmailConnectionError(t *testing.T) {
	listener, _ := net.Listen("tcp", "127.0.0.1:0")
	port := listener.Addr().(*net.TCPAddr).Port
	_ = listener.Close()

	settings := config.SMTP{Host: "127.0.0.1", Port: port, From: "errors@example.com"}

	err := Email{SMTP: settings, To: "ops@example.com"}.Send(context.Background(), sample)

	if err == nil || !strings.Contains(err.Error(), "smtp connect 127.0.0.1:"+strconv.Itoa(port)) {
		t.Fatalf("got %v", err)
	}
}

func TestVerificationCodeEmail(t *testing.T) {
	settings, sessions := fixtures.FakeSMTP(t)

	if err := (Email{SMTP: settings, To: "new@example.com"}).SendVerificationCode(context.Background(), "Backend <team>", "482913", 15*time.Minute); err != nil {
		t.Fatal(err)
	}

	session := <-sessions
	subject, text, html := fixtures.ParseEmail(t, session.Data)

	if !strings.Contains(strings.Join(session.Commands, "\n"), "RCPT TO:<new@example.com>") || !strings.Contains(subject, "482913") ||
		!strings.Contains(text, "482913") || !strings.Contains(html, "482913") || !strings.Contains(html, "Backend &lt;team&gt;") || !strings.Contains(text, "15 minutes") {
		t.Fatalf("subject %q\ntext:\n%s\nhtml:\n%s", subject, text, html)
	}
}

func TestEmailIssueLinkSetting(t *testing.T) {
	settings, sessions := fixtures.FakeSMTP(t)
	factory := Factory{SMTP: settings}
	notification := Notification{Kind: NewIssue, Project: "backend", Title: "Example issue", Level: "warning", IssueURL: "https://errors.example.com/issues/issue005"}

	for _, hidden := range []bool{false, true} {
		channel, err := factory.Channel(store.Connection{Type: config.ChannelEmail, Address: "ops@example.com", HideIssueLink: hidden})

		if err != nil {
			t.Fatal(err)
		}

		if err := channel.Send(context.Background(), notification); err != nil {
			t.Fatal(err)
		}

		_, text, html := fixtures.ParseEmail(t, (<-sessions).Data)

		if strings.Contains(text, notification.IssueURL) == hidden || strings.Contains(html, notification.IssueURL) == hidden || strings.Contains(html, "Open issue") == hidden {
			t.Fatalf("hide link %v: unexpected link visibility", hidden)
		}

		if !strings.Contains(text, notification.Title) || !strings.Contains(html, notification.Title) {
			t.Fatal("message content must remain in both parts")
		}

		if err := channel.SendSummary(context.Background(), Summary{Count: 1, Window: time.Minute, Items: []Notification{notification}}); err != nil {
			t.Fatal(err)
		}

		_, text, html = fixtures.ParseEmail(t, (<-sessions).Data)

		if strings.Contains(text, notification.IssueURL) == hidden || strings.Contains(html, notification.IssueURL) == hidden {
			t.Fatalf("hide link %v: unexpected summary link visibility", hidden)
		}
	}
}

func TestPromptFileName(t *testing.T) {
	testCases := []struct {
		notification Notification
		want         string
	}{
		{Notification{Project: "backend", Level: "error", IssueID: "issue006"}, "backend-error-issue006.md"},
		{Notification{Project: "Web App / SSR", Level: "fatal", IssueID: "issue012"}, "web-app-ssr-fatal-issue012.md"},
		{Notification{Project: "../../etc", IssueID: "issue003"}, "etc-error-issue003.md"},
	}

	for _, testCase := range testCases {
		if got := promptFileName(testCase.notification); got != testCase.want {
			t.Errorf("promptFileName(%+v) = %q, want %q", testCase.notification, got, testCase.want)
		}
	}
}
