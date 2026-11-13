package ui_test

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"html"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
)

const verifier = "01234567890123456789012345678901234567890123456789"

func authorizationParams(t *testing.T, server *testServer) url.Values {
	t.Helper()

	request := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(`{"client_name":"Codex <test>","redirect_uris":["http://127.0.0.1:44891/callback"]}`))
	request.Header.Set("Content-Type", "application/json")
	response := server.send(request)

	if response.Code != http.StatusCreated {
		t.Fatalf("register: %d %s", response.Code, response.Body)
	}

	var client struct {
		ID string `json:"client_id"`
	}

	if err := json.Unmarshal(response.Body.Bytes(), &client); err != nil {
		t.Fatal(err)
	}

	challenge := sha256.Sum256([]byte(verifier))

	return url.Values{
		"client_id": {client.ID}, "redirect_uri": {"http://127.0.0.1:44891/callback"},
		"response_type": {"code"}, "state": {"opaque state & return"},
		"code_challenge_method": {"S256"}, "code_challenge": {base64.RawURLEncoding.EncodeToString(challenge[:])},
		"resource": {"http://example.com/mcp"},
	}
}

func consentForm(t *testing.T, server *testServer, params url.Values) url.Values {
	t.Helper()

	response := server.get("/oauth/authorize?" + params.Encode())

	if response.Code != http.StatusOK {
		t.Fatalf("consent: %d %s", response.Code, response.Body)
	}

	if !strings.Contains(response.Body.String(), "Codex &lt;test&gt;") {
		t.Fatal("client name missing or unescaped")
	}

	if !strings.Contains(response.Header().Get("Content-Security-Policy"), "form-action 'self' http://127.0.0.1:44891") {
		t.Fatal("callback blocked by CSP")
	}

	form := url.Values{}

	for _, input := range regexp.MustCompile(`<input type="hidden" name="([^"]+)" value="([^"]*)">`).FindAllStringSubmatch(response.Body.String(), -1) {
		form.Add(html.UnescapeString(input[1]), html.UnescapeString(input[2]))
	}

	form.Set("decision", "allow")

	return form
}

func tokenRequest(t *testing.T, server *testServer, form url.Values, status int) map[string]any {
	t.Helper()

	response := server.post("/oauth/token", form)

	if response.Code != status {
		t.Fatalf("token: %d %s", response.Code, response.Body)
	}

	var tokens map[string]any

	if err := json.Unmarshal(response.Body.Bytes(), &tokens); err != nil {
		t.Fatal(err)
	}

	return tokens
}

func TestOAuthAuthorizationFlow(t *testing.T) {
	server := newTestServer(t, password)
	params := authorizationParams(t, server)
	response := server.get("/oauth/authorize?" + params.Encode())

	if response.Code != http.StatusSeeOther {
		t.Fatalf("expected login redirect: %d", response.Code)
	}

	loginURL, err := url.Parse(response.Header().Get("Location"))

	if err != nil {
		t.Fatal(err)
	}

	next := loginURL.Query().Get("next")

	if next != "/oauth/authorize?"+params.Encode() {
		t.Fatalf("lost authorization parameters: %s", next)
	}

	response = server.post("/login", url.Values{"username": {"admin"}, "password": {password}, "next": {next}})

	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != next {
		t.Fatalf("login lost return URL: %d %s", response.Code, response.Header().Get("Location"))
	}

	for _, cookie := range response.Result().Cookies() {
		if cookie.Name == "em_session" {
			server.cookie = cookie
		}
	}

	form := consentForm(t, server, params)
	response = server.post("/oauth/authorize", form)

	if response.Code != http.StatusSeeOther {
		t.Fatalf("approve: %d %s", response.Code, response.Body)
	}

	callback, err := url.Parse(response.Header().Get("Location"))

	if err != nil {
		t.Fatal(err)
	}

	if callback.Host != "127.0.0.1:44891" || callback.Query().Get("state") != params.Get("state") || callback.Query().Get("iss") != "http://example.com" || callback.Query().Get("code") == "" {
		t.Fatalf("invalid callback: %s", callback)
	}

	exchange := url.Values{"grant_type": {"authorization_code"}, "client_id": {params.Get("client_id")}, "redirect_uri": {params.Get("redirect_uri")}, "code": {callback.Query().Get("code")}, "code_verifier": {verifier}, "resource": {params.Get("resource")}}
	tokens := tokenRequest(t, server, exchange, http.StatusOK)
	tokenRequest(t, server, exchange, http.StatusBadRequest)

	checkAccess := func(token string, status int) {
		t.Helper()

		request := httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
		request.Header.Set("Content-Type", "application/json")
		request.Header.Set("Accept", "application/json, text/event-stream")
		request.Header.Set("Authorization", "Bearer "+token)
		response := server.send(request)

		if response.Code != status {
			t.Fatalf("MCP: %d %s", response.Code, response.Body)
		}

		if status == http.StatusOK && !strings.Contains(response.Body.String(), "get_issue") {
			t.Fatalf("MCP did not list tools: %s", response.Body)
		}
	}
	checkAccess(tokens["access_token"].(string), http.StatusOK)
	refresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {params.Get("client_id")}, "refresh_token": {tokens["refresh_token"].(string)}}
	rotated := tokenRequest(t, server, refresh, http.StatusOK)

	checkAccess(tokens["access_token"].(string), http.StatusUnauthorized)
	checkAccess(rotated["access_token"].(string), http.StatusOK)
	tokenRequest(t, server, refresh, http.StatusBadRequest)
	checkAccess(rotated["access_token"].(string), http.StatusUnauthorized)
}

func TestOAuthConsentRejectsInvalidRequests(t *testing.T) {
	server := newTestServer(t, password)
	params := authorizationParams(t, server)
	server.login()
	form := consentForm(t, server, params)

	for _, test := range []struct {
		name   string
		change func(url.Values)
		origin string
		status int
	}{
		{"missing CSRF", func(form url.Values) { form.Del("csrf_token") }, "", http.StatusForbidden},
		{"tampered callback", func(form url.Values) { form.Set("redirect_uri", "http://127.0.0.1:44891/other") }, "", http.StatusForbidden},
		{"foreign origin", func(url.Values) {}, "http://evil.example", http.StatusForbidden},
		{"missing decision", func(form url.Values) { form.Del("decision") }, "", http.StatusBadRequest},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := url.Values{}

			for key, values := range form {
				copy[key] = append([]string(nil), values...)
			}

			test.change(copy)
			response := server.post("/oauth/authorize", copy, "Origin", test.origin)

			if response.Code != test.status {
				t.Fatalf("got %d %s", response.Code, response.Body)
			}
		})
	}

	for _, test := range []struct{ name, key, value string }{
		{"unregistered client", "client_id", "missing"},
		{"unregistered callback", "redirect_uri", "http://127.0.0.1:44891/other"},
		{"fragment callback", "redirect_uri", "http://127.0.0.1:44891/callback#fragment"},
		{"missing PKCE", "code_challenge", ""},
		{"plain PKCE", "code_challenge_method", "plain"},
		{"wrong resource", "resource", "https://other.example/mcp"},
	} {
		t.Run(test.name, func(t *testing.T) {
			copy := url.Values{}

			for key, values := range params {
				copy[key] = append([]string(nil), values...)
			}

			copy.Set(test.key, test.value)
			response := server.get("/oauth/authorize?" + copy.Encode())

			if response.Code != http.StatusBadRequest || response.Header().Get("Location") != "" {
				t.Fatalf("invalid request: %d %s", response.Code, response.Body)
			}
		})
	}

	form.Set("decision", "deny")
	response := server.post("/oauth/authorize", form)
	callback, err := url.Parse(response.Header().Get("Location"))

	if err != nil {
		t.Fatal(err)
	}

	if response.Code != http.StatusSeeOther || callback.Query().Get("error") != "access_denied" || callback.Query().Get("state") != params.Get("state") || callback.Query().Get("code") != "" {
		t.Fatalf("deny: %d %s", response.Code, callback)
	}
}

func TestOAuthRejectsWrongPKCE(t *testing.T) {
	server := newTestServer(t, password)
	params := authorizationParams(t, server)
	server.login()
	response := server.post("/oauth/authorize", consentForm(t, server, params))
	callback, err := url.Parse(response.Header().Get("Location"))

	if err != nil {
		t.Fatal(err)
	}

	result := tokenRequest(t, server, url.Values{"grant_type": {"authorization_code"}, "client_id": {params.Get("client_id")}, "redirect_uri": {params.Get("redirect_uri")}, "code": {callback.Query().Get("code")}, "code_verifier": {"wrong"}}, http.StatusBadRequest)

	if result["error"] != "invalid_grant" {
		t.Fatalf("wrong PKCE: %v", result)
	}
}
