package contracts

import "time"

// AgentsView is the MCP page: how to connect a coding agent.
type AgentsView struct {
	URL                  string // the MCP endpoint
	Client               string // the selected tab
	Tabs                 []AgentTab
	ClaudeCommand        string // `claude mcp add ...`
	CodexCommand         string // `codex mcp add ...`
	ProjectConfig        string // .mcp.json, for a team sharing the setup in a repository
	PersonalTokenSet     bool
	PersonalToken        string // just created: shown once
	PersonalTokenPreview string // the same, masked
	Connections          []AgentConnection
	Tools                []AgentTool
	IPsRestricted        bool // ALLOWED_IPS limits who may connect
	Unencrypted          bool // tokens would cross the network without TLS
}

type AgentTool struct {
	Name        string
	Description string
	ReadOnly    bool
}

// AgentTab selects the instructions for one MCP client.
type AgentTab struct {
	ID     string
	Label  string
	Active bool
}

// AgentConnection describes an authorized client without exposing its tokens.
type AgentConnection struct {
	ID         int64
	Name       string
	Kind       string
	Connected  time.Time
	LastActive *time.Time
}
