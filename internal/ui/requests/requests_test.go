package requests

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/store"
)

func postForm(form url.Values) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = request.ParseForm()

	return request
}

func TestReadBulkIssues(t *testing.T) {
	ids, problem := ReadBulkIssues(postForm(url.Values{"issue": {"3", "7"}}))

	if problem != "" || !slices.Equal(ids, []int64{3, 7}) {
		t.Errorf("ReadBulkIssues = %v, %q", ids, problem)
	}

	if _, problem := ReadBulkIssues(postForm(url.Values{"issue": {"3", "x"}})); problem != "invalid issue id" {
		t.Errorf("bad id: problem = %q", problem)
	}

	many := make([]string, maxBulkIssues+1)

	for index := range many {
		many[index] = fmt.Sprint(index + 1)
	}

	if _, problem := ReadBulkIssues(postForm(url.Values{"issue": many})); problem == "" {
		t.Errorf("%d issues accepted, want the limit of %d", len(many), maxBulkIssues)
	}
}

func TestReadProjectFormLevels(t *testing.T) {
	recording := config.Recording{EventLevels: []string{"error", "fatal"}} // log levels nil: all

	var project store.Project

	problems := ReadProjectForm(postForm(url.Values{
		"name": {"api"}, "event_level": {"fatal", "error"}, "log_level": config.LogLevels,
	}), &project, true, recording, "#2563eb")

	if len(problems) > 0 {
		t.Fatalf("problems = %v", problems)
	}

	if project.EventLevels != nil || project.LogLevels != nil {
		t.Errorf("defaults stored as %v / %v, want nil (follow the defaults)", project.EventLevels, project.LogLevels)
	}

	// Nothing checked records nothing; levels the list does not know are dropped.
	ReadProjectForm(postForm(url.Values{"name": {"api"}, "event_level": {"warning", "bogus"}}), &project, true, recording, "#2563eb")

	if project.EventLevels == nil || !slices.Equal(*project.EventLevels, []string{"warning"}) {
		t.Errorf("event levels = %v, want [warning]", project.EventLevels)
	}

	if project.LogLevels == nil || len(*project.LogLevels) != 0 {
		t.Errorf("log levels = %v, want an empty list (record none)", project.LogLevels)
	}
}

func TestReadProjectFormValidation(t *testing.T) {
	project := store.Project{Name: "backend", Color: "#123456"}
	problems := ReadProjectForm(postForm(url.Values{
		"name": {"Not Valid"}, "retention_events": {"30d"}, "retention_logs": {"soon"}, "retention_issues": {""},
		"retention_events_per_issue": {"-1"}, "retention_logs_per_message": {"0"}, "color": {"#2563EB"},
	}), &project, false, config.Recording{}, "#2563eb")

	for _, field := range []string{"retention_logs", "retention_events_per_issue"} {
		if problems[field] == "" {
			t.Errorf("%s accepted, want an error", field)
		}
	}

	if _, ok := problems["name"]; ok || project.Name != "backend" {
		t.Errorf("name of a project from env read from the form: %q, %v", project.Name, problems)
	}

	if project.RetentionEvents == nil || *project.RetentionEvents != 30*24*time.Hour {
		t.Errorf("retention_events = %v, want 30 days", project.RetentionEvents)
	}

	if project.RetentionIssues != nil {
		t.Errorf("empty retention_issues = %v, want nil (the default)", *project.RetentionIssues)
	}

	if project.RetentionLogsPerMessage == nil || *project.RetentionLogsPerMessage != 0 {
		t.Errorf("retention_logs_per_message = %v, want 0 (keep all)", project.RetentionLogsPerMessage)
	}

	// The palette color is stored as "" so the project keeps following the palette.
	if project.Color != "" {
		t.Errorf("color = %q, want empty for the palette color", project.Color)
	}

	if problems := ReadProjectForm(postForm(url.Values{"name": {"Not Valid"}}), &project, true, config.Recording{}, ""); problems["name"] == "" {
		t.Errorf("invalid editable name accepted")
	}
}

func TestSafeNext(t *testing.T) {
	for next, want := range map[string]string{
		"/logs?level=error":    "/logs?level=error",
		"":                     "/issues",
		"https://example.com/": "/issues",
		"//example.com/":       "/issues",
		"/\\example.com":       "/issues",
	} {
		if got := SafeNext(next); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", next, got, want)
		}
	}
}
