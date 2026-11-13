package mcp

import (
	"context"
	"embed"
	"fmt"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
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

var fetchSkillTool = &sdk.Tool{
	Name:        "fetch_skill",
	Description: "Detailed guidance by name: fetch-data, analyze-errors or update-issue-status. Only when the server instructions do not settle the task; reuse a skill already loaded.",
	Annotations: readOnly,
}

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

func (handler *Handler) fetchSkill(_ context.Context, _ *sdk.CallToolRequest, input fetchSkillInput) (*sdk.CallToolResult, any, error) {
	for _, skill := range skills {
		if skill.name != input.Name {
			continue
		}

		content, err := skillFiles.ReadFile("skills/" + skill.name + "/SKILL.md")

		if err != nil {
			return handler.failed("fetch_skill", err)
		}

		return text(string(content))
	}

	return handler.failed("fetch_skill", invalid("unknown skill %q: use fetch-data, analyze-errors or update-issue-status", input.Name))
}
