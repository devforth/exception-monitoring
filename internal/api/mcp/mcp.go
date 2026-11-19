// Package mcp is the MCP server at /mcp for coding agents: the tools are the
// endpoints of package catalog a user's role allows, plus fetch_skill.
package mcp

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/devforth/exception-monitoring/internal/api/catalog"
	"github.com/devforth/exception-monitoring/internal/api/oauth"
	"github.com/devforth/exception-monitoring/internal/platform/config"
	"github.com/devforth/exception-monitoring/internal/server/access"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Path is where agents connect.
const Path = "/mcp"

type Handler struct {
	oauth  *oauth.Server
	logger *slog.Logger
	store  store.Store
	http   http.Handler
}

// roles are the roles a server is built for.
var roles = []string{store.RoleSuperadmin, store.RoleAdmin, store.RoleUser}

// New builds the MCP servers from the catalog; appName (APP_NAME) names them to agents.
func New(logger *slog.Logger, database store.Store, endpoints []catalog.Endpoint, authorization *oauth.Server, appName, version string) *Handler {
	handler := &Handler{oauth: authorization, logger: logger, store: database}
	servers := map[string]*sdk.Server{}

	for _, role := range roles {
		server := sdk.NewServer(&sdk.Implementation{Name: config.AppSlug(appName), Title: appName, Version: version},
			&sdk.ServerOptions{Instructions: serverInstructions(appName)})

		// Any user of the role: the tools depend on the role only.
		for _, endpoint := range agentEndpoints(endpoints, store.User{ID: store.NewID(), Role: role}) {
			server.AddTool(tool(endpoint), handler.run(endpoint))
		}

		servers[role] = server
	}

	handler.http = sdk.NewStreamableHTTPHandler(func(r *http.Request) *sdk.Server {
		caller, ok := r.Context().Value(callerKey{}).(catalog.Call)

		if !ok {
			return nil // the SDK answers 400; Protect has let no such request through
		}

		return servers[caller.Viewer.Role]
	}, &sdk.StreamableHTTPOptions{
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

// agentEndpoints are the tools viewer may use: the catalog without the
// REST-only endpoints and those of other roles, plus fetch_skill.
func agentEndpoints(endpoints []catalog.Endpoint, viewer store.User) []catalog.Endpoint {
	var tools []catalog.Endpoint

	for _, endpoint := range append(endpoints, fetchSkill) {
		if endpoint.AllowedFor(catalog.Call{Viewer: viewer}) {
			tools = append(tools, endpoint)
		}
	}

	return tools
}

func tool(endpoint catalog.Endpoint) *sdk.Tool {
	annotations := &sdk.ToolAnnotations{ReadOnlyHint: endpoint.ReadOnly}

	if !endpoint.ReadOnly {
		annotations.IdempotentHint = endpoint.Idempotent
		annotations.DestructiveHint = &endpoint.Destructive
	}

	return &sdk.Tool{Name: endpoint.Name, Description: endpoint.ToolDescription(), InputSchema: endpoint.InputSchema, Annotations: annotations}
}

// run calls the endpoint for the user the agent acts for. Errors become
// tool errors the agent reads: input mistakes and missing records as they
// are, anything else logged and reported without details.
func (handler *Handler) run(endpoint catalog.Endpoint) sdk.ToolHandler {
	return func(ctx context.Context, request *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		call, ok := ctx.Value(callerKey{}).(catalog.Call)

		if !ok {
			return handler.failed(endpoint, catalog.ErrNotFound)
		}

		var input map[string]any

		if len(request.Params.Arguments) > 0 {
			if err := json.Unmarshal(request.Params.Arguments, &input); err != nil {
				return handler.failed(endpoint, catalog.Invalid("arguments must be a JSON object"))
			}
		}

		output, err := endpoint.Run(ctx, call, input)

		if err != nil {
			return handler.failed(endpoint, err)
		}

		if text, ok := output.(catalog.Text); ok {
			return textResult(string(text)), nil
		}

		encoded, err := json.Marshal(output)

		if err != nil {
			return handler.failed(endpoint, err)
		}

		return textResult(string(encoded)), nil
	}
}

func textResult(content string) *sdk.CallToolResult {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: content}}}
}

func (handler *Handler) failed(endpoint catalog.Endpoint, err error) (*sdk.CallToolResult, error) {
	message, _, ok := catalog.Outcome(err)

	if !ok {
		handler.logger.Error("mcp tool failed", "tool", endpoint.Name, "err", err)

		message = "internal error"
	}

	result := textResult(message)
	result.IsError = true

	return result, nil
}

// Register adds the endpoint and the open OAuth endpoints to router, wrapped
// with protect (the IP allowlist). The endpoint itself needs a token. No
// method in its pattern: the transport answers GET and DELETE itself.
func (handler *Handler) Register(router *http.ServeMux, protect func(http.Handler) http.Handler) {
	router.Handle(Path, protect(handler.oauth.Protect(handler.withCaller(handler.http))))
	handler.oauth.Register(router, protect)
}

type callerKey struct{}

// withCaller reads who the agent acts for: the grant's user as stored now
// (a changed role applies at once), and the server's address as the agent
// reached it, for DSNs. It runs after oauth.Protect has checked the token.
func (handler *Handler) withCaller(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		grant, ok := oauth.GrantOf(auth.TokenInfoFromContext(r.Context()))

		if !ok {
			http.Error(w, "no grant", http.StatusUnauthorized)

			return
		}

		viewer, err := handler.store.GetUser(r.Context(), grant.UserID)

		if err != nil {
			handler.logger.Warn("mcp grant without its user", "grant", grant.ID, "err", err)
			http.Error(w, "unknown user", http.StatusUnauthorized)

			return
		}

		address := access.PublicURL(r, handler.oauth.PublicURL, handler.oauth.TrustedProxies)
		call := catalog.Call{Viewer: viewer, Actor: viewer.Actor(store.SourceMCP), ForAgent: true, Address: address}

		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), callerKey{}, call)))
	})
}
