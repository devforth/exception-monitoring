// Package mcp is the MCP server (Model Context Protocol): coding agents such
// as Claude Code connect to /mcp to list issues, read an error with its stack
// trace, breadcrumbs and the logs of its request, search logs and change an
// issue's status.
//
// It is served over Streamable HTTP by the main server, behind ALLOWED_IPS.
// Agents sign in with OAuth or send the personal token (package oauth); both
// are managed on the MCP page of the UI. The server is stateless: every
// request stands alone, so nothing has to be kept between calls or cleaned up.
//
// What a tool returns goes to the agent's model provider, so it follows the
// rules of the agent prompt (package agentprompt): event details come from
// agentprompt.Build, and free text in lists (titles, log lines) goes
// through agentprompt.Redact. Raw payloads and log attributes are not exposed.
package mcp

import (
	"log/slog"
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/devforth/exception-monitoring/internal/config"
	"github.com/devforth/exception-monitoring/internal/oauth"
	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/services"
)

// Path is where agents connect.
const Path = "/mcp"

type Handler struct {
	oauth    *oauth.Server
	logger   *slog.Logger
	store    *store.Store
	issues   *services.Issues
	projects config.ProjectList
	http     http.Handler
}

// New builds the MCP server; appName (APP_NAME) names it to agents.
func New(logger *slog.Logger, database *store.Store, projects config.ProjectList, authorization *oauth.Server, appName, version string) *Handler {
	handler := &Handler{oauth: authorization, logger: logger, store: database, issues: services.NewIssues(database), projects: projects}
	server := sdk.NewServer(&sdk.Implementation{Name: config.MCPServerName(appName), Title: appName, Version: version},
		&sdk.ServerOptions{Instructions: serverInstructions(appName)})

	handler.addTools(server)

	handler.http = sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return server }, &sdk.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		// The SDK rejects requests to a localhost address with another Host
		// (DNS rebinding of a local server without auth). Behind a reverse
		// proxy on the same machine every request looks like that, and the
		// endpoint requires a token. oauth.Protect additionally validates
		// browser Origins against PUBLIC_URL/the request, including GET.
		DisableLocalhostProtection: true,
	})

	return handler
}

// Register adds the endpoint and the open OAuth endpoints to router, wrapped
// with protect (the IP allowlist). The endpoint itself needs a token. No
// method in its pattern: the transport answers GET and DELETE itself.
func (handler *Handler) Register(router *http.ServeMux, protect func(http.Handler) http.Handler) {
	router.Handle(Path, protect(handler.oauth.Protect(handler.http)))
	handler.oauth.Register(router, protect)
}
