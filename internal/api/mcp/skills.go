package mcp

import (
	"context"
	"embed"
	"fmt"
	"strings"

	"github.com/devforth/exception-monitoring/internal/api/catalog"
)

//go:embed instructions.md skills/*/SKILL.md
var skillFiles embed.FS

type agentSkill struct {
	name        string
	description string
}

// skills are guides an agent loads on demand (fetch_skill), so the
// instructions sent with every connection stay short. Each covers one topic;
// the instructions list them with these descriptions.
var skills = []agentSkill{
	{"fetch-data", "filters, paging and level names of the list tools"},
	{"analyze-errors", "investigating an issue and matching it to the repository"},
	{"update-issue-status", "what the statuses mean and when to change them"},
}

// fetchSkill is a tool for agents only, not part of the REST API.
var fetchSkill = catalog.Define(catalog.Endpoint{
	Name:        "fetch_skill",
	Description: "Detailed guidance by name: fetch-data, analyze-errors or update-issue-status. Only when the server instructions do not settle the task; reuse a skill already loaded.",
	ReadOnly:    true,
	AgentsOnly:  true,
}, readSkill)

// serverInstructions is instructions.md with {{app}} replaced by appName.
func serverInstructions(appName string) string {
	instructions, err := skillFiles.ReadFile("instructions.md")

	// Embedded at build time: a missing file is a programming error.
	if err != nil {
		panic(err)
	}

	var catalog strings.Builder
	catalog.WriteString(strings.ReplaceAll(string(instructions), "{{app}}", appName))

	for _, skill := range skills {
		fmt.Fprintf(&catalog, "  - %s: %s\n", skill.name, skill.description)
	}

	return catalog.String()
}

type fetchSkillInput struct {
	Name string `json:"name" jsonschema:"fetch-data, analyze-errors or update-issue-status."`
}

func readSkill(_ context.Context, _ catalog.Call, input fetchSkillInput) (catalog.Text, error) {
	for _, skill := range skills {
		if skill.name != input.Name {
			continue
		}

		content, err := skillFiles.ReadFile("skills/" + skill.name + "/SKILL.md")

		if err != nil {
			return "", err
		}

		return catalog.Text(content), nil
	}

	return "", catalog.Invalid("unknown skill %q: use fetch-data, analyze-errors or update-issue-status", input.Name)
}
