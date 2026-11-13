package oauth

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/devforth/exception-monitoring/internal/access"
	"github.com/devforth/exception-monitoring/internal/store"
)

func testAuthorization(t *testing.T) (*Server, http.Handler) {
	t.Helper()
	database, err := store.Open(context.Background(), t.TempDir())

	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})

	server := &Server{Store: database, PublicURL: "https://errors.example.com", ResourcePath: "/mcp", Now: func() time.Time { return time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC) }}
	router := http.NewServeMux()
	server.Register(router, func(handler http.Handler) http.Handler { return handler })
	router.Handle("/mcp", server.Protect(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) })))

	return server, router
}

func oauthRequest(handler http.Handler, method, path, body string, headers ...string) *httptest.ResponseRecorder {
	request := httptest.NewRequest(method, "https://errors.example.com"+path, strings.NewReader(body))
	request.RemoteAddr = "127.0.0.1:1234"

	for index := 0; index+1 < len(headers); index += 2 {
		request.Header.Add(headers[index], headers[index+1])
	}

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	return response
}

func testClient(t *testing.T, server *Server) store.OAuthClient {
	t.Helper()

	client := store.OAuthClient{ID: "test-client", Name: "Test client", RedirectURIs: []string{"http://127.0.0.1:44891/callback"}, CreatedAt: server.now()}

	if err := server.Store.RegisterOAuthClient(context.Background(), client); err != nil {
		t.Fatal(err)
	}

	return client
}

const testVerifier = "01234567890123456789012345678901234567890123456789"

func testCode(t *testing.T, server *Server, client store.OAuthClient) url.Values {
	t.Helper()

	request := httptest.NewRequest(http.MethodGet, "https://errors.example.com/oauth/authorize", nil)
	params := url.Values{"client_id": {client.ID}, "redirect_uri": {client.RedirectURIs[0]}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {pkceChallenge(testVerifier)}, "resource": {"https://errors.example.com/mcp"}}
	checked, err := server.CheckRequest(context.Background(), request, params)

	if err != nil {
		t.Fatal(err)
	}

	callback, err := server.Approve(context.Background(), request, checked)

	if err != nil {
		t.Fatal(err)
	}

	target, err := url.Parse(callback)

	if err != nil {
		t.Fatal(err)
	}

	return url.Values{"grant_type": {"authorization_code"}, "client_id": {client.ID}, "redirect_uri": {client.RedirectURIs[0]}, "code": {target.Query().Get("code")}, "code_verifier": {testVerifier}}
}

func exchange(t *testing.T, handler http.Handler, form url.Values, status int) map[string]any {
	t.Helper()

	response := oauthRequest(handler, http.MethodPost, TokenPath, form.Encode(), "Content-Type", "application/x-www-form-urlencoded")

	if response.Code != status {
		t.Fatalf("token endpoint: %d %s", response.Code, response.Body)
	}

	var result map[string]any

	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}

	return result
}

func TestOriginProtection(t *testing.T) {
	server, handler := testAuthorization(t)
	token, err := server.NewPersonalToken(context.Background())

	if err != nil {
		t.Fatal(err)
	}

	for _, origin := range []string{"https://evil.example", "null", "http://errors.example.com", "https://user@errors.example.com", "https://errors.example.com/path", "https://errors.example.com?query=1"} {
		for _, path := range []string{"/mcp", RegisterPath, TokenPath, "/.well-known/oauth-protected-resource/mcp"} {
			method := http.MethodPost

			if strings.HasPrefix(path, "/.well-known/") {
				method = http.MethodGet
			}

			response := oauthRequest(handler, method, path, "", "Origin", origin, "Authorization", "Bearer "+token)

			if response.Code != http.StatusForbidden {
				t.Errorf("%s origin %q: %d", path, origin, response.Code)
			}
		}

		response := oauthRequest(handler, http.MethodGet, "/mcp", "", "Origin", origin, "Authorization", "Bearer "+token)

		if response.Code != http.StatusForbidden {
			t.Errorf("GET origin %q: %d", origin, response.Code)
		}
	}

	for _, headers := range [][]string{
		{"Authorization", "Bearer " + token},
		{"Authorization", "bearer " + token, "Origin", "https://errors.example.com"},
		{"Authorization", "Bearer " + token, "Origin", "https://ERRORS.example.com:443"},
	} {
		if response := oauthRequest(handler, http.MethodPost, "/mcp", "", headers...); response.Code != http.StatusNoContent {
			t.Errorf("valid origin: %d %s", response.Code, response.Body)
		}
	}

	response := oauthRequest(handler, http.MethodPost, "/mcp", "", "Origin", "https://errors.example.com", "Origin", "https://evil.example", "Authorization", "Bearer "+token)

	if response.Code != http.StatusForbidden {
		t.Fatal("multiple Origins accepted")
	}

	response = oauthRequest(handler, http.MethodPost, "/mcp", "", "Sec-Fetch-Site", "cross-site", "Authorization", "Bearer "+token)

	if response.Code != http.StatusForbidden {
		t.Fatal("cross-site fetch without Origin accepted")
	}

	response = oauthRequest(handler, http.MethodPost, "/mcp", "", "Authorization", "Bearer "+token, "Authorization", "Bearer "+token)

	if response.Code != http.StatusUnauthorized {
		t.Fatal("multiple credentials accepted")
	}
}

func TestOAuthRedirectValidation(t *testing.T) {
	for _, uri := range []string{
		"https://client.example/callback", "http://localhost:3000/callback", "http://127.0.0.1:44891/callback", "http://[::1]:3000/callback",
	} {
		if err := checkRedirectURI(uri); err != nil {
			t.Errorf("valid URI %q: %v", uri, err)
		}
	}

	for _, uri := range []string{
		"http://client.example/callback", "https:/callback", "https://user:password@client.example/callback",
		"https://client.example/callback#fragment", "https://client.example/callback#", "javascript:alert(1)", "data:text/html,test", "intent://callback", "file:///tmp/callback", "vscode://callback",
		"https://client.example:99999/callback", "https://client.example/callback\\other", "https://client.example;form-action/callback",
	} {
		if err := checkRedirectURI(uri); err == nil {
			t.Errorf("unsafe URI accepted: %q", uri)
		}
	}

	if !redirectMatches("http://127.0.0.1:2222/callback", "http://127.0.0.1:1111/callback") {
		t.Fatal("loopback port variation rejected")
	}

	for _, requested := range []string{"http://127.0.0.1:2222/callback#fragment", "http://127.0.0.1:2222/other", "http://localhost:1111/callback", "http://127.0.0.1:2222/callback?other=1"} {
		if redirectMatches(requested, "http://127.0.0.1:1111/callback") {
			t.Errorf("callback mismatch accepted: %s", requested)
		}
	}
}

func TestOAuthRejectsMalformedRegistration(t *testing.T) {
	_, handler := testAuthorization(t)

	for _, body := range []string{
		`{"redirect_uris":["https://client.example/callback"]} {}`,
		`{"redirect_uris":["https://client.example/callback"]}` + strings.Repeat(" ", maxBody),
		`{"redirect_uris":["intent://callback"]}`,
	} {
		response := oauthRequest(handler, http.MethodPost, RegisterPath, body, "Content-Type", "application/json")

		if response.Code != http.StatusBadRequest {
			t.Errorf("malformed registration: %d %s", response.Code, response.Body)
		}
	}
}

func TestOAuthRateLimits(t *testing.T) {
	_, handler := testAuthorization(t)

	for index := 0; index < 31; index++ {
		response := oauthRequest(handler, http.MethodPost, RegisterPath, `{}`, "Content-Type", "application/json")

		if index == 30 && (response.Code != http.StatusTooManyRequests || response.Header().Get("Retry-After") == "") {
			t.Fatalf("registration flood not limited: %d", response.Code)
		}
	}

	for index := 0; index < 121; index++ {
		response := oauthRequest(handler, http.MethodPost, TokenPath, "grant_type=unsupported", "Content-Type", "application/x-www-form-urlencoded")

		if index == 120 && response.Code != http.StatusTooManyRequests {
			t.Fatalf("token flood not limited: %d", response.Code)
		}
	}

	limiter := requestLimit{perMinute: 1}
	now := time.Date(2026, 11, 13, 12, 0, 0, 0, time.UTC)
	address := netip.MustParseAddr("127.0.0.1")

	if !limiter.allow(address, now) || limiter.allow(address, now) || !limiter.allow(address, now.Add(time.Minute)) {
		t.Fatal("rate window did not reset")
	}
}

func TestPKCEAndDuplicateParameters(t *testing.T) {
	server, handler := testAuthorization(t)
	client := testClient(t, server)
	form := testCode(t, server, client)
	form.Add("client_id", "other-client")
	exchange(t, handler, form, http.StatusBadRequest)
	form.Set("client_id", client.ID)
	form.Set("resource", "https://other.example/mcp")
	exchange(t, handler, form, http.StatusBadRequest)
	form.Del("resource")
	exchange(t, handler, form, http.StatusOK)

	for _, verifier := range []string{"short", strings.Repeat("x", 129), strings.Repeat("x", 42) + "!"} {
		if validVerifier(verifier) {
			t.Errorf("invalid verifier accepted: %q", verifier)
		}
	}

	params := url.Values{"client_id": {client.ID}, "redirect_uri": {client.RedirectURIs[0]}, "response_type": {"code"}, "code_challenge_method": {"S256"}}

	for _, challenge := range []string{"", "short", base64.RawURLEncoding.EncodeToString(make([]byte, 31)), strings.Repeat("a", 43) + "="} {
		params.Set("code_challenge", challenge)

		if _, err := server.CheckRequest(context.Background(), httptest.NewRequest(http.MethodGet, "https://errors.example.com", nil), params); err == nil {
			t.Errorf("invalid challenge accepted: %q", challenge)
		}
	}
}

func TestRefreshReplayRevokesGrant(t *testing.T) {
	server, handler := testAuthorization(t)
	client := testClient(t, server)
	tokens := exchange(t, handler, testCode(t, server, client), http.StatusOK)
	refresh := url.Values{"grant_type": {"refresh_token"}, "client_id": {client.ID}, "refresh_token": {tokens["refresh_token"].(string)}}
	rotated := exchange(t, handler, refresh, http.StatusOK)
	// Wrong client and random tokens must not revoke a legitimate grant.
	refresh.Set("client_id", "other-client")
	exchange(t, handler, refresh, http.StatusBadRequest)

	if _, err := server.Authenticate(context.Background(), rotated["access_token"].(string)); err != nil {
		t.Fatal("wrong client revoked grant", err)
	}

	refresh.Set("client_id", client.ID)
	refresh.Set("refresh_token", "emcp_rt_made-up")
	exchange(t, handler, refresh, http.StatusBadRequest)

	if _, err := server.Authenticate(context.Background(), rotated["access_token"].(string)); err != nil {
		t.Fatal("made-up token revoked grant", err)
	}

	pending := testCode(t, server, client)

	refresh.Set("refresh_token", tokens["refresh_token"].(string))
	exchange(t, handler, refresh, http.StatusBadRequest)

	if _, err := server.Authenticate(context.Background(), rotated["access_token"].(string)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("replayed refresh did not revoke access token", err)
	}

	refresh.Set("refresh_token", rotated["refresh_token"].(string))
	exchange(t, handler, refresh, http.StatusBadRequest)
	exchange(t, handler, pending, http.StatusBadRequest)
}

func TestDisconnectInvalidatesPendingCodes(t *testing.T) {
	server, handler := testAuthorization(t)
	client := testClient(t, server)
	tokens := exchange(t, handler, testCode(t, server, client), http.StatusOK)
	pending := testCode(t, server, client)
	grant, err := server.Authenticate(context.Background(), tokens["access_token"].(string))

	if err != nil {
		t.Fatal(err)
	}

	if err := server.Store.DeleteGrant(context.Background(), grant.ID); err != nil {
		t.Fatal(err)
	}

	exchange(t, handler, pending, http.StatusBadRequest)
}

func TestOAuthExpiry(t *testing.T) {
	server, handler := testAuthorization(t)
	client := testClient(t, server)
	code := testCode(t, server, client)
	now := server.now()
	server.Now = func() time.Time { return now.Add(codeTTL) }

	exchange(t, handler, code, http.StatusBadRequest)
	tokens := exchange(t, handler, testCode(t, server, client), http.StatusOK)
	issued := server.now()
	server.Now = func() time.Time { return issued.Add(accessTokenTTL) }

	if _, err := server.Authenticate(context.Background(), tokens["access_token"].(string)); !errors.Is(err, store.ErrNotFound) {
		t.Fatal("expired access token accepted", err)
	}

	server.Now = func() time.Time { return issued.Add(refreshTokenTTL) }

	exchange(t, handler, url.Values{"grant_type": {"refresh_token"}, "client_id": {client.ID}, "refresh_token": {tokens["refresh_token"].(string)}}, http.StatusBadRequest)
}

func TestOAuthEndpointsRespectIPAllowlist(t *testing.T) {
	server, _ := testAuthorization(t)
	router := http.NewServeMux()
	server.Register(router, func(next http.Handler) http.Handler {
		return access.RequireAllowedIP([]netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}, nil, next)
	})

	for _, path := range []string{RegisterPath, TokenPath, "/.well-known/oauth-protected-resource/mcp"} {
		method := http.MethodPost

		if strings.HasPrefix(path, "/.well-known/") {
			method = http.MethodGet
		}

		response := oauthRequest(router, method, path, "", "X-Forwarded-For", "10.0.0.1")

		if response.Code != http.StatusForbidden {
			t.Errorf("IP allowlist bypassed at %s: %d", path, response.Code)
		}
	}
}

func TestConcurrentTokenExchanges(t *testing.T) {
	for _, refresh := range []bool{false, true} {
		t.Run(map[bool]string{false: "authorization code", true: "refresh token"}[refresh], func(t *testing.T) {
			server, handler := testAuthorization(t)
			client := testClient(t, server)
			form := testCode(t, server, client)

			if refresh {
				tokens := exchange(t, handler, form, http.StatusOK)
				form = url.Values{"grant_type": {"refresh_token"}, "client_id": {client.ID}, "refresh_token": {tokens["refresh_token"].(string)}}
			}

			results := make(chan int, 2)

			for range 2 {
				go func() {
					response := oauthRequest(handler, http.MethodPost, TokenPath, form.Encode(), "Content-Type", "application/x-www-form-urlencoded")

					results <- response.Code
				}()
			}

			first, second := <-results, <-results

			if first+second != http.StatusOK+http.StatusBadRequest || first == second {
				t.Fatalf("concurrent exchange: %d, %d", first, second)
			}

			if refresh {
				grants, err := server.Store.ListGrants(context.Background(), server.now())

				if err != nil || len(grants) != 0 {
					t.Fatalf("concurrent replay did not revoke grant: %v %+v", err, grants)
				}
			}
		})
	}
}

func TestOriginBehindTrustedProxy(t *testing.T) {
	server, _ := testAuthorization(t)
	server.PublicURL = ""
	server.TrustedProxies = []netip.Prefix{netip.MustParsePrefix("10.0.0.0/8")}
	request := httptest.NewRequest(http.MethodPost, "http://errors.example.com/mcp", nil)
	request.RemoteAddr = "10.0.0.1:1234"
	request.Header.Set("X-Forwarded-Proto", "https")
	request.Header.Set("Origin", "https://errors.example.com")

	if !server.sameOrigin(request) {
		t.Fatal("trusted HTTPS proxy rejected")
	}

	request.RemoteAddr = "192.0.2.1:1234"

	if server.sameOrigin(request) {
		t.Fatal("untrusted proxy header accepted")
	}

	server.PublicURL = "https://errors.example.com/monitoring"
	request.Host = "internal-proxy:8000"

	if !server.sameOrigin(request) {
		t.Fatal("configured public origin rejected behind proxy")
	}
}
