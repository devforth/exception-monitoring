package oauth

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"strings"
	"testing"
)

func TestClientIDURL(t *testing.T) {
	for clientID, want := range map[string]bool{
		"https://claude.ai/oauth/claude-code-client-metadata": true,
		"https://example.com/client.json?v=1":                 true,
		"0123456789abcdef0123456789abcdef":                    false, // registered
		"http://example.com/client.json":                      false,
		"https://example.com":                                 false,
		"https://example.com/":                                false,
		"https://user:pass@example.com/client.json":           false,
		"https://example.com/client.json#part":                false,
		"https://127.0.0.1/client.json":                       false,
		"https://[::1]/client.json":                           false,
		"https://example.com/a b":                             false,
	} {
		if got := isClientIDURL(clientID); got != want {
			t.Errorf("isClientIDURL(%q) = %v, want %v", clientID, got, want)
		}
	}
}

// The client_id is chosen by whoever connects: fetching it must not reach
// this server's own network.
func TestMetadataFetchReachesOnlyPublicAddresses(t *testing.T) {
	for address, want := range map[string]bool{
		"8.8.8.8": true, "2606:4700::1111": true,
		"127.0.0.1": false, "10.1.2.3": false, "172.16.0.1": false, "192.168.1.1": false, "169.254.169.254": false,
		"100.64.0.1": false, "0.0.0.0": false, "::1": false, "fe80::1": false, "fc00::1": false, "::ffff:10.0.0.1": false, "224.0.0.1": false,
	} {
		if got := publicAddress(netip.MustParseAddr(address)); got != want {
			t.Errorf("publicAddress(%s) = %v, want %v", address, got, want)
		}
	}

	// A real fetch from a local server is refused at the connection.
	local := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{}`)) }))
	t.Cleanup(local.Close)

	if response, err := metadataClient().Get(local.URL + "/client.json"); err == nil {
		_ = response.Body.Close()

		t.Fatal("the metadata client reached a loopback address")
	}
}

// metadataServer publishes documents over TLS; the returned HTTP client
// reaches it for any host name, as if example.com resolved to it.
func metadataServer(t *testing.T, documents map[string]any) *http.Client {
	t.Helper()

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		document, ok := documents[r.URL.Path]

		if !ok {
			http.NotFound(w, r)

			return
		}

		if text, ok := document.(string); ok {
			_, _ = w.Write([]byte(text))

			return
		}

		_ = json.NewEncoder(w).Encode(document)
	}))
	t.Cleanup(server.Close)

	transport := server.Client().Transport.(*http.Transport).Clone()
	transport.DialContext = func(ctx context.Context, network, _ string) (net.Conn, error) {
		return (&net.Dialer{}).DialContext(ctx, network, server.Listener.Addr().String())
	}
	transport.TLSClientConfig = &tls.Config{RootCAs: transport.TLSClientConfig.RootCAs, ServerName: "example.com"}

	return &http.Client{Transport: transport}
}

func TestClientMetadataDocument(t *testing.T) {
	server, handler := testAuthorization(t)
	clientID := "https://example.com/client.json"
	redirect := "http://127.0.0.1:44891/callback"
	document := func(changes map[string]any) map[string]any {
		values := map[string]any{"client_id": clientID, "client_name": "Claude Code", "redirect_uris": []string{redirect}, "token_endpoint_auth_method": "none"}

		for key, value := range changes {
			values[key] = value
		}

		return values
	}
	server.MetadataClient = metadataServer(t, map[string]any{
		"/client.json":     document(nil),
		"/other-id.json":   document(nil), // names client.json as its client_id
		"/secret.json":     document(map[string]any{"client_id": "https://example.com/secret.json", "token_endpoint_auth_method": "client_secret_basic"}),
		"/plain-http.json": document(map[string]any{"client_id": "https://example.com/plain-http.json", "redirect_uris": []string{"http://evil.example/callback"}}),
		"/large.json":      `{"client_id":"https://example.com/large.json","client_name":"` + strings.Repeat("x", 6000) + `"}`,
	})

	params := url.Values{"client_id": {clientID}, "redirect_uri": {redirect}, "response_type": {"code"}, "code_challenge_method": {"S256"}, "code_challenge": {pkceChallenge(testVerifier)}}
	request := httptest.NewRequest(http.MethodGet, "https://errors.example.com/oauth/authorize", nil)
	checked, err := server.CheckRequest(context.Background(), request, params)

	if err != nil {
		t.Fatal(err)
	}

	if checked.ClientName != "Claude Code" || checked.ClientSite() != "example.com" {
		t.Fatalf("checked = %q from %q", checked.ClientName, checked.ClientSite())
	}

	// Nothing is stored for it before the user allows it.
	if _, err := server.Store.GetOAuthClient(context.Background(), clientID); err == nil {
		t.Fatal("a metadata client was stored")
	}

	callback, err := server.Approve(context.Background(), request, checked, testUserID(t, server))

	if err != nil {
		t.Fatal(err)
	}

	target, _ := url.Parse(callback)
	exchange(t, handler, url.Values{"grant_type": {"authorization_code"}, "client_id": {clientID}, "redirect_uri": {redirect}, "code": {target.Query().Get("code")}, "code_verifier": {testVerifier}}, http.StatusOK)

	grants, err := server.Grants(context.Background(), testUserID(t, server))

	if err != nil || len(grants) != 1 || grants[0].Name != "Claude Code" || grants[0].ClientID != clientID {
		t.Fatalf("grants = %+v, %v", grants, err)
	}

	// A redirect the document does not list, and documents that must be refused.
	params.Set("redirect_uri", "http://127.0.0.1:44891/other")

	if _, err := server.CheckRequest(context.Background(), request, params); err == nil {
		t.Error("a redirect_uri the document does not list was accepted")
	}

	params.Set("redirect_uri", redirect)

	for _, path := range []string{"/other-id.json", "/secret.json", "/plain-http.json", "/large.json", "/missing.json"} {
		params.Set("client_id", "https://example.com"+path)

		if _, err := server.CheckRequest(context.Background(), request, params); err == nil {
			t.Errorf("%s accepted", path)
		}
	}
}
