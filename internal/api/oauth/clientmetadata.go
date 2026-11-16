package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Client ID Metadata Documents (the MCP way for clients to identify
// without registering): the client_id is an https URL of a JSON document
// the client publishes with its name and redirect_uris. The server reads it
// at every authorization and keeps nothing, so, unlike the open
// registration, it cannot be filled up. Clients that do not support it
// register (registration.go).

const (
	maxMetadataBytes = 5 * 1024
	metadataTimeout  = 5 * time.Second
)

// specialPurpose are the networks a metadata URL must not reach: the
// client_id is chosen by whoever connects, so fetching it must not become a
// way to reach this server's own network (SSRF). Loopback, private and
// link-local addresses are checked with netip's methods.
var specialPurpose = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("240.0.0.0/4"),
	netip.MustParsePrefix("64:ff9b::/96"),
	netip.MustParsePrefix("100::/64"),
	netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"),
}

func publicAddress(address netip.Addr) bool {
	address = address.Unmap()

	if !address.IsGlobalUnicast() || address.IsPrivate() {
		return false
	}

	for _, network := range specialPurpose {
		if network.Contains(address) {
			return false
		}
	}

	return true
}

// isClientIDURL: the client_id names a metadata document (an https URL
// with a host name and a path), not a registered client.
func isClientIDURL(clientID string) bool {
	parsed, err := url.Parse(clientID)

	if err != nil || parsed.Scheme != "https" || parsed.String() != clientID || parsed.User != nil || parsed.Fragment != "" ||
		parsed.Path == "" || parsed.Path == "/" || parsed.Hostname() == "" {
		return false
	}

	_, isAddress := netip.ParseAddr(parsed.Hostname())

	return isAddress != nil
}

// metadataClient fetches metadata documents: it connects only to public
// addresses, checked after DNS resolution (so a name that resolves to a
// private address, even on a second lookup, is refused), follows no
// redirects, and gives up after metadataTimeout.
func metadataClient() *http.Client {
	dialer := &net.Dialer{
		Timeout: metadataTimeout,
		Control: func(_, address string, _ syscall.RawConn) error {
			host, _, err := net.SplitHostPort(address)

			if err != nil {
				return err
			}

			if parsed, err := netip.ParseAddr(host); err != nil || !publicAddress(parsed) {
				return fmt.Errorf("%s is not a public address", host)
			}

			return nil
		},
	}

	return &http.Client{
		Timeout:       metadataTimeout,
		Transport:     &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: metadataTimeout, Proxy: nil},
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

// metadataDocument is what the client publishes at its client_id.
type metadataDocument struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
}

// errMetadata is what the agent and the consent page are told when the
// document cannot be read; why stays out of it (addresses, statuses and
// parse errors would tell whoever chose the client_id what this server can
// reach).
var errMetadata = oauthError("invalid_client", "the client metadata document at client_id could not be read")

// fetchClient reads a client from its metadata document.
func (server *Server) fetchClient(ctx context.Context, clientID string) (store.OAuthClient, error) {
	client := server.MetadataClient

	if client == nil {
		client = metadataClient()
	}

	ctx, cancel := context.WithTimeout(ctx, metadataTimeout)

	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, clientID, nil)

	if err != nil {
		return store.OAuthClient{}, errMetadata
	}

	request.Header.Set("Accept", "application/json")

	response, err := client.Do(request)

	if err != nil {
		return store.OAuthClient{}, errMetadata
	}

	defer func() { _ = response.Body.Close() }()

	body, err := io.ReadAll(io.LimitReader(response.Body, maxMetadataBytes+1))

	if err != nil || response.StatusCode != http.StatusOK || len(body) > maxMetadataBytes {
		return store.OAuthClient{}, errMetadata
	}

	var document metadataDocument

	if err := json.Unmarshal(body, &document); err != nil {
		return store.OAuthClient{}, errMetadata
	}

	return clientFromDocument(clientID, document)
}

// clientFromDocument checks a document like a registration: the same
// redirect_uri policy and name limit.
func clientFromDocument(clientID string, document metadataDocument) (store.OAuthClient, error) {
	switch {
	case document.ClientID != clientID:
		return store.OAuthClient{}, oauthError("invalid_client", "client_id in the client metadata document does not match its URL")
	case document.TokenEndpointAuthMethod != "none":
		return store.OAuthClient{}, oauthError("invalid_client", `the client metadata document must have token_endpoint_auth_method "none"`)
	case len(document.RedirectURIs) == 0 || len(document.RedirectURIs) > maxRedirectURIs:
		return store.OAuthClient{}, oauthError("invalid_client", "the client metadata document must have 1 to 10 redirect_uris")
	}

	for _, uri := range document.RedirectURIs {
		if err := checkRedirectURI(uri); err != nil {
			return store.OAuthClient{}, err
		}
	}

	return store.OAuthClient{ID: clientID, Name: clientName(document.ClientName), RedirectURIs: document.RedirectURIs}, nil
}

// clientName is a client's name as shown on the consent page and the MCP page.
func clientName(name string) string {
	name = strings.TrimSpace(name)

	if name == "" {
		return "Unnamed agent"
	}

	if utf8.RuneCountInString(name) > maxClientName {
		return string([]rune(name)[:maxClientName])
	}

	return name
}

// client finds the agent behind a client_id: its metadata document, or a
// registration. store.ErrNotFound when it was never registered.
func (server *Server) client(ctx context.Context, clientID string) (store.OAuthClient, error) {
	if isClientIDURL(clientID) {
		return server.fetchClient(ctx, clientID)
	}

	client, err := server.Store.GetOAuthClient(ctx, clientID)

	if errors.Is(err, store.ErrNotFound) {
		return client, oauthError("invalid_client", "this agent is not registered here: add the server to the agent again")
	}

	return client, err
}
