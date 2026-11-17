package presenters

import (
	"encoding/json"
	"fmt"

	"github.com/devforth/exception-monitoring/internal/storage/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
)

// AgentsInput is what the MCP page shows.
type AgentsInput struct {
	Endpoint      string       // the MCP endpoint
	ServerName    string       // what agents list it under (config.AppSlug)
	Client        string       // the selected tab (?client=)
	Token         string       // a personal token just created, or empty
	Owner         store.User   // whose grants are listed
	Owners        []store.User // whom the viewer may choose
	Grants        []store.Grant
	TokenSet      bool // the viewer has a personal token
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

	if input.Token != "" {
		view.SetupPrompt = setupPrompt(input.ServerName, input.Endpoint, input.Token)
	}

	view.OwnerID = input.Owner.ID

	// A choice only for those who may see someone else's agents.
	if len(input.Owners) > 1 {
		for _, user := range input.Owners {
			view.Owners = append(view.Owners, contracts.AgentOwner{ID: user.ID, Name: user.DisplayName()})
		}
	}

	for _, tab := range agentTabs {
		tab.Active = tab.ID == view.Client
		view.Tabs = append(view.Tabs, tab)
	}

	view.PersonalTokenSet = input.TokenSet

	for _, grant := range input.Grants {
		// An unused personal token is not a connected app yet.
		if grant.Kind == store.GrantToken && grant.LastUsedAt == nil {
			continue
		}

		view.Connections = append(view.Connections, contracts.AgentConnection{
			ID: grant.ID, Name: grant.Name, Personal: grant.Kind == store.GrantToken, Connected: grant.CreatedAt, LastActive: grant.LastUsedAt,
		})
	}

	return view
}

// setupPrompt asks a coding agent to add the MCP server with a personal
// token itself and to check that it works. The token is shown once, so is
// the prompt.
func setupPrompt(serverName, endpoint, token string) string {
	header := "Authorization: Bearer " + token

	return fmt.Sprintf(`Add the MCP server %[1]q to your own MCP configuration: it gives you the errors and logs of my applications.

- Transport: Streamable HTTP
- URL: %[2]s
- Header: %[3]s

Add it at user level, not in a file of a repository: the token is a secret.
- Claude Code: claude mcp add --transport http --scope user %[1]s %[2]s --header %[4]q
- Codex, in ~/.codex/config.toml:
  [mcp_servers.%[1]s]
  url = %[2]q
  http_headers = { Authorization = %[5]q }
- Another client: the same URL and header in its MCP settings.

Then check the connection: call the list_projects tool and tell me which projects you see. If you must be restarted to load the server, tell me. If it is not reachable, show me the error and do not try other URLs.
Never repeat the token in your answers, files or commits.
`, serverName, endpoint, header, header, "Bearer "+token)
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
