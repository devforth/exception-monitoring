package services

import (
	"net/url"
	"slices"
	"testing"

	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

func TestProjectFormLevels(t *testing.T) {
	recording := config.Recording{EventLevels: []string{"error", "fatal"}} // log levels nil: all

	var project store.Project

	problems := readProjectForm(url.Values{
		"name": {"api"}, "event_level": {"fatal", "error"}, "log_level": config.LogLevels,
	}, &project, true, recording, "#2563eb")

	if len(problems) > 0 {
		t.Fatalf("problems = %v", problems)
	}

	if project.EventLevels != nil || project.LogLevels != nil {
		t.Errorf("defaults stored as %v / %v, want nil (follow the defaults)", project.EventLevels, project.LogLevels)
	}

	// Nothing checked records nothing; levels the list does not know are dropped.
	readProjectForm(url.Values{"name": {"api"}, "event_level": {"warning", "bogus"}}, &project, true, recording, "#2563eb")

	if project.EventLevels == nil || !slices.Equal(*project.EventLevels, []string{"warning"}) {
		t.Errorf("event levels = %v, want [warning]", project.EventLevels)
	}

	if project.LogLevels == nil || len(*project.LogLevels) != 0 {
		t.Errorf("log levels = %v, want an empty list (record none)", project.LogLevels)
	}
}

func TestProjectFormValidation(t *testing.T) {
	project := store.Project{Name: "backend", Color: "#123456"}
	problems := readProjectForm(url.Values{
		"name": {"Not Valid"}, "color": {"#2563EB"},
	}, &project, false, config.Recording{}, "#2563eb")

	if _, ok := problems["name"]; ok || project.Name != "backend" {
		t.Errorf("name of a project from env read from the form: %q, %v", project.Name, problems)
	}

	// The palette color is stored as "" so the project keeps following the palette.
	if project.Color != "" {
		t.Errorf("color = %q, want empty for the palette color", project.Color)
	}

	if problems := readProjectForm(url.Values{"name": {"Not Valid"}}, &project, true, config.Recording{}, ""); problems["name"] == "" {
		t.Errorf("invalid editable name accepted")
	}
}
