package presenters

import (
	"encoding/json"

	"github.com/devforth/exception-monitoring/internal/mcp"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
)

// AgentsInput is what the MCP page shows.
type AgentsInput struct {
	Endpoint      string // the MCP endpoint
	ServerName    string // what agents list it under (config.MCPServerName)
	Client        string // the selected tab (?client=)
	Token         string // a personal token just created, or empty
	Grants        []store.Grant
	Tools         []mcp.ToolInfo
	IPsRestricted bool
	Unencrypted   bool
}

// agentTabs are the clients the page has instructions for; the first is the default.
var agentTabs = []contracts.AgentTab{{ID: "claude", Label: "Claude Code"}, {ID: "codex", Label: "Codex"}, {ID: "other", Label: "Other"}}

// Agents presents the MCP page.
func Agents(input AgentsInput) contracts.AgentsView {
	view := contracts.AgentsView{
		URL: input.Endpoint, Client: AgentClient(input.Client), IPsRestricted: input.IPsRestricted, Unencrypted: input.Unencrypted,
		ClaudeCommand: "claude mcp add --transport http " + input.ServerName + " " + input.Endpoint,
		CodexCommand:  "codex mcp add " + input.ServerName + " --url " + input.Endpoint,
		ProjectConfig: projectConfig(input.ServerName, input.Endpoint),
		PersonalToken: input.Token, PersonalTokenPreview: maskToken(input.Token),
	}

	for _, tab := range agentTabs {
		tab.Active = tab.ID == view.Client
		view.Tabs = append(view.Tabs, tab)
	}

	for _, grant := range input.Grants {
		if grant.Kind == store.GrantToken {
			view.PersonalTokenSet = true

			// An unused personal token is not a connected app yet.
			if grant.LastUsedAt == nil {
				continue
			}
		}

		view.Connections = append(view.Connections, contracts.AgentConnection{
			ID: grant.ID, Name: grant.Name, Kind: grant.Kind, Connected: grant.CreatedAt, LastActive: grant.LastUsedAt,
		})
	}

	for _, tool := range input.Tools {
		view.Tools = append(view.Tools, contracts.AgentTool(tool))
	}

	return view
}

// maskToken shows the start and end of a token, enough to recognize it.
func maskToken(token string) string {
	if len(token) <= 18 {
		return ""
	}

	return token[:10] + "****" + token[len(token)-8:]
}

// projectConfig is the .mcp.json of Claude Code, for a repository: each
// developer then signs in in their own browser.
func projectConfig(serverName, endpoint string) string {
	type server struct {
		Type string `json:"type"`
		URL  string `json:"url"`
	}

	config := map[string]map[string]server{"mcpServers": {serverName: {Type: "http", URL: endpoint}}}
	encoded, _ := json.MarshalIndent(config, "", "  ")

	return string(encoded)
}

// AgentClient accepts only the known tabs (from the query or a form).
func AgentClient(client string) string {
	for _, tab := range agentTabs {
		if tab.ID == client {
			return client
		}
	}

	return agentTabs[0].ID
}
