package presenters

import (
	"fmt"
	"slices"
	"testing"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/services"
	"github.com/devforth/exception-monitoring/internal/storage/store"
	"github.com/devforth/exception-monitoring/internal/ui/lists"
)

func testConnections() Connections {
	return Connections{
		// SMTP but no SECRET_KEY: only email can be used.
		Notifications: config.Notifications{SMTP: config.SMTP{Host: "smtp.example.com"}},
		Projects:      config.Projects{{ID: 1, Name: "backend"}},
		ProjectName:   func(projectID int) string { return fmt.Sprintf("project %d", projectID) },
	}
}

func TestConnectionStatus(t *testing.T) {
	presenter := testConnections()

	for _, test := range []struct {
		verified, active bool
		status, class    string
	}{
		{false, true, "awaiting confirmation", "awaiting"},
		{true, true, "active", "active"},
		{true, false, "inactive", "inactive"},
	} {
		row := presenter.Row(store.Connection{Type: config.ChannelEmail, Address: "dev@example.com", Verified: test.verified, Active: test.active, ProjectIDs: []int{7}})

		if row.Status != test.status || row.StatusClass != test.class {
			t.Errorf("verified %v, active %v: status %q (%s), want %q (%s)", test.verified, test.active, row.Status, row.StatusClass, test.status, test.class)
		}

		if row.Destination != "dev@example.com" || len(row.Projects) != 1 || row.Projects[0] != "project 7" {
			t.Errorf("row = %+v, want the address and the project name", row)
		}
	}
}

func TestEmailStepState(t *testing.T) {
	presenter := testConnections()

	for _, test := range []struct {
		name       string
		connection store.Connection
		state      string
		fixed      bool
	}{
		{"new", store.Connection{Type: config.ChannelEmail}, "input", false},
		{"existing, not confirmed", store.Connection{ID: 3, Type: config.ChannelEmail}, "unconfirmed", true},
		{"existing, confirmed", store.Connection{ID: 3, Type: config.ChannelEmail, Verified: true}, "verified", true},
	} {
		view := presenter.Form(test.connection)

		if view.Email.State != test.state || view.Email.Fixed != test.fixed {
			t.Errorf("%s: state %q, fixed %v; want %q, %v", test.name, view.Email.State, view.Email.Fixed, test.state, test.fixed)
		}
	}

	// A new email form shown again: a confirmed address stays confirmed, a
	// taken one goes back to the address field with a link.
	confirmed := presenter.FormAgain(store.Connection{Type: config.ChannelEmail, DestinationID: 4}, map[string]string{"levels": "x"}, "", nil, "dev@example.com")

	if confirmed.Email.State != "verified" || confirmed.Email.Address != "dev@example.com" || confirmed.Errors["levels"] == "" {
		t.Errorf("form again = %+v, want the confirmed step and the errors", confirmed.Email)
	}

	existing := &services.Duplicate{ID: 9, Name: "dev"}
	taken := presenter.FormAgain(store.Connection{Type: config.ChannelEmail, Address: "dev@example.com", DestinationID: 4}, nil, "", existing, "dev@example.com")

	if taken.Email.State != "input" || taken.Email.Duplicate != existing || taken.Duplicate != nil {
		t.Errorf("taken address: step %+v, form duplicate %v; want the link in the email step", taken.Email, taken.Duplicate)
	}
}

func TestNewConnectionDefaults(t *testing.T) {
	presenter := testConnections()

	// Without SECRET_KEY only email can be used: it is preselected and gets issue links.
	connection := presenter.NewConnection()

	if connection.Type != config.ChannelEmail || connection.HideIssueLink || !connection.AllProjects || !connection.Active {
		t.Errorf("new connection = %+v, want email, all projects, active, with issue links", connection)
	}

	presenter.Notifications.SMTP = config.SMTP{}
	presenter.Notifications.SecretKey = "test-secret-key-test-secret-key-0123"

	if chat := presenter.NewConnection(); chat.Type != config.ChannelTelegram || !chat.HideIssueLink {
		t.Errorf("new connection without SMTP = %+v, want a chat without issue links", chat)
	}

	// The levels are those recorded by default (RECORD_EVENT_LEVELS); all when all are recorded.
	presenter.Recording = config.Recording{EventLevels: []string{"warning", "error"}}

	if levels := presenter.NewConnection().Levels; !slices.Equal(levels, []string{"warning", "error"}) {
		t.Errorf("levels = %v, want the recorded ones", levels)
	}

	presenter.Recording = config.Recording{}

	if levels := presenter.NewConnection().Levels; !slices.Equal(levels, config.Levels) {
		t.Errorf("levels = %v, want every level", levels)
	}
}

func testProjects() Projects {
	return Projects{
		Recording: config.Recording{EventLevels: []string{"error", "fatal"}},
		FromEnv:   func(projectID int) bool { return projectID == 1 },
		Color:     func(project store.Project) string { return config.DefaultProjectColor(project.ID) },
	}
}

func TestProjectForm(t *testing.T) {
	presenter := testProjects()
	none := []string{}
	view := presenter.Form(store.Project{ID: 2, Name: "api", EventLevels: &[]string{"warning"}, LogLevels: &none})

	if view.New || view.FromEnv || view.Color != config.DefaultProjectColor(2) {
		t.Errorf("view = %+v", view)
	}

	if want := lists.Options(lists.EventLevels, []string{"warning"}); fmt.Sprint(view.EventLevels) != fmt.Sprint(want) {
		t.Errorf("event levels = %v, want only warning checked, highest first", view.EventLevels)
	}

	for _, option := range view.LogLevels {
		if option.Selected {
			t.Errorf("log level %s checked, want none (an empty list records nothing)", option.Name)
		}
	}

	// A new project: the defaults checked (nil log levels record every level).
	fresh := presenter.Form(store.Project{})

	if !fresh.New || fresh.FromEnv {
		t.Errorf("new project view = %+v", fresh)
	}

	for _, option := range fresh.EventLevels {
		if want := option.Name == "error" || option.Name == "fatal"; option.Selected != want {
			t.Errorf("event level %s checked = %v for a new project, want %v", option.Name, option.Selected, want)
		}
	}

	for _, option := range fresh.LogLevels {
		if !option.Selected {
			t.Errorf("log level %s not checked for a new project, want every level", option.Name)
		}
	}

	info := presenter.Info(config.Project{ID: 1, Name: "backend"}, "dsn", 4)

	if !info.FromEnv || info.UnresolvedIssues != 4 {
		t.Errorf("info = %+v", info)
	}
}

func TestStorage(t *testing.T) {
	storage := Storage(1500, config.Retention{MaxDatabaseBytes: 1000})

	if storage.Percent != 100 || storage.Limit == "" || storage.CleanupStop != config.CleanupStopPercent {
		t.Errorf("storage = %+v, want the percent capped at 100", storage)
	}

	if unlimited := Storage(1500, config.Retention{}); unlimited.Limit != "" || unlimited.Percent != 0 {
		t.Errorf("no size limit: %+v", unlimited)
	}
}
