package tests

import (
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"

	"github.com/devforth/exception-monitoring/internal/platform/config"
)

// The MCP page tells how to connect an agent, at the address agents reach,
// without showing the token.
func TestAgentsPage(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"

	server := newTestServerWith(t, func(settings *config.Config) {
		settings.APIToken = token
		settings.Notifications.PublicURL = "https://errors.example.com/monitoring"
	})
	server.login()

	page := html.UnescapeString(server.page("/agents"))

	for _, want := range []string{
		"claude mcp add --transport http exception-monitoring https://errors.example.com/monitoring/mcp",
		`"url": "https://errors.example.com/monitoring/mcp"`,
		"codex mcp add exception-monitoring --url https://errors.example.com/monitoring/mcp",
		"get_issue", "update_issue_status", `data-copy="mcp-command"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}

	if strings.Contains(page, token) {
		t.Error("the page shows the API token")
	}

	if strings.Contains(page, "the MCP server is closed") {
		t.Error("warns about a missing token that is set")
	}

	// OAuth works independently of the REST API token.
	closed := newTestServerWith(t, func(*config.Config) {})
	closed.login()

	if page := closed.page("/agents"); strings.Contains(page, "the MCP server is closed") || !strings.Contains(page, "http://example.com/mcp") {
		t.Errorf("without a token:\n%s", page)
	}

	// Plain HTTP to another machine: codes and tokens could be read on the way.
	if !strings.Contains(closed.page("/agents"), "cross the network unencrypted") {
		t.Error("no warning about plain HTTP")
	}

	if strings.Contains(page, "cross the network unencrypted") {
		t.Error("warns about plain HTTP behind https")
	}
}

func TestAgentTabs(t *testing.T) {
	server := newTestServer(t, password)
	server.login()

	for _, client := range []string{"claude", "codex", "other"} {
		t.Run(client, func(t *testing.T) {
			page := server.page("/agents?client=" + client)

			if !strings.Contains(page, `aria-selected="true" tabindex="0" data-mcp-tab="`+client+`"`) {
				t.Fatal("wrong active tab")
			}

			for _, panel := range []string{"claude", "codex", "other"} {
				match := regexp.MustCompile(`<div id="mcp-panel-` + panel + `"[^>]*>`).FindString(page)

				if match == "" || strings.Contains(match, " hidden") != (panel != client) {
					t.Fatalf("wrong panel visibility: %s", match)
				}
			}

			if !strings.Contains(page, "Connected apps") || !strings.Contains(page, "No apps connected yet.") {
				t.Fatal("missing shared connections list")
			}
		})
	}

	if page := server.page("/agents?client=unknown"); !strings.Contains(page, `aria-selected="true" tabindex="0" data-mcp-tab="claude"`) {
		t.Fatal("invalid tab did not fall back")
	}
}

func agentAccess(t *testing.T, server *testServer, token string, status int) {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Authorization", "Bearer "+token)
	response := server.send(request)

	if response.Code != status {
		t.Fatalf("MCP access: %d %s", response.Code, response.Body)
	}
}

func TestPersonalAgentTokenLifecycle(t *testing.T) {
	server := newTestServer(t, password)

	if response := server.post("/agents/token", url.Values{}); response.Code != http.StatusSeeOther {
		t.Fatal("token creation did not require login")
	}

	server.login()

	form := url.Values{}

	if response := server.post("/agents/token", form, "Origin", "http://evil.example"); response.Code != http.StatusForbidden {
		t.Fatal("cross-origin token creation accepted")
	}

	create := func() string {
		t.Helper()

		response := server.post("/agents/token", form, "HX-Request", "true")

		if response.Code != http.StatusOK || strings.Contains(response.Body.String(), "<!doctype") {
			t.Fatalf("token fragment: %d %s", response.Code, response.Body)
		}

		match := personalTokenField.FindStringSubmatch(response.Body.String())

		if len(match) != 2 {
			t.Fatal("missing personal token")
		}

		// The setup prompt for an agent carries the new token, once.
		body := html.UnescapeString(response.Body.String())
		command := `claude mcp add --transport http --scope user exception-monitoring http://example.com/mcp --header "Authorization: Bearer ` + match[1] + `"`

		if !strings.Contains(body, `<textarea id="mcp-setup-prompt" hidden>`) || !strings.Contains(body, command) || !strings.Contains(body, "Copy setup prompt") {
			t.Fatalf("setup prompt:\n%s", body)
		}

		return match[1]
	}
	token := create()
	page := server.page("/agents?client=other")

	if !strings.Contains(page, "No apps connected yet.") || strings.Contains(page, "/disconnect") || !strings.Contains(page, "Regenerate token") {
		t.Fatal("unused personal token should be available for regeneration without appearing as a connected app")
	}

	agentAccess(t, server, token, http.StatusOK)
	page = server.page("/agents?client=other")

	if strings.Contains(page, token) || strings.Contains(page, "mcp-setup-prompt") || !strings.Contains(page, "Regenerate token") || !strings.Contains(page, "<time datetime=") {
		t.Fatal("token leaked or connection metadata missing")
	}

	replacement := create()

	if replacement == token {
		t.Fatal("token was not replaced")
	}

	agentAccess(t, server, token, http.StatusUnauthorized)
	agentAccess(t, server, replacement, http.StatusOK)
	page = server.page("/agents?client=other")
	action := disconnectAction.FindStringSubmatch(page)

	if len(action) != 2 {
		t.Fatal("missing disconnect button")
	}

	if response := server.post(action[1], url.Values{}, "Origin", "http://evil.example"); response.Code != http.StatusForbidden {
		t.Fatal("cross-origin disconnect accepted")
	}

	response := server.post(action[1], url.Values{"client": {"other"}})

	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/agents?client=other" {
		t.Fatalf("disconnect redirect: %d %s", response.Code, response.Body)
	}

	agentAccess(t, server, replacement, http.StatusUnauthorized)

	if page := server.page("/agents"); !strings.Contains(page, "No apps connected yet.") {
		t.Fatal("disconnected token still listed")
	}
}

func TestDisconnectOAuthAgent(t *testing.T) {
	server := newTestServer(t, password)
	params := authorizationParams(t, server)
	server.login()
	response := server.post("/oauth/authorize", consentForm(t, server, params))
	callback, err := url.Parse(response.Header().Get("Location"))

	if err != nil {
		t.Fatal(err)
	}

	tokens := tokenRequest(t, server, url.Values{
		"grant_type": {"authorization_code"}, "client_id": {params.Get("client_id")},
		"redirect_uri": {params.Get("redirect_uri")}, "code": {callback.Query().Get("code")}, "code_verifier": {verifier},
	}, http.StatusOK)
	page := server.page("/agents?client=codex")

	if !strings.Contains(page, "Codex &lt;test&gt;") || !strings.Contains(page, "Not used yet") {
		t.Fatal("connected OAuth app missing")
	}

	agentAccess(t, server, tokens["access_token"].(string), http.StatusOK)
	page = server.page("/agents?client=codex")

	if strings.Contains(page, "Not used yet") {
		t.Fatal("last activity was not recorded")
	}

	action := disconnectAction.FindStringSubmatch(page)

	if len(action) != 2 {
		t.Fatal("missing disconnect button")
	}

	response = server.post(action[1], url.Values{"client": {"codex"}})

	if response.Code != http.StatusSeeOther {
		t.Fatalf("disconnect: %d", response.Code)
	}

	agentAccess(t, server, tokens["access_token"].(string), http.StatusUnauthorized)
	tokenRequest(t, server, url.Values{"grant_type": {"refresh_token"}, "client_id": {params.Get("client_id")}, "refresh_token": {tokens["refresh_token"].(string)}}, http.StatusBadRequest)
}

// APP_NAME names the panel and the MCP server; agents get it with dashes.
func TestAppName(t *testing.T) {
	server := newTestServerWith(t, func(settings *config.Config) { settings.AppName = "Acme Errors" })
	server.login()
	server.ingestFixtures()

	page := html.UnescapeString(server.page("/agents"))

	for _, want := range []string{
		`<a class="brand" href="/issues">Acme Errors</a>`, "<title>MCP · Acme Errors</title>",
		"claude mcp add --transport http acme-errors http://example.com/mcp", "codex mcp add acme-errors --url",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("missing %q", want)
		}
	}

	issue := "/issues/" + issueIDs(server.page("/issues"))[0]

	if page := html.UnescapeString(server.page(issue)); !strings.Contains(page, `data-copy-text="Acme Errors MCP: get_issue {"issue_id":`) {
		t.Error("the MCP request does not carry the app name")
	}
}
