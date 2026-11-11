package access

import (
	"net/http"
	"net/http/httptest"
	"net/netip"
	"testing"
)

func networks(values ...string) []netip.Prefix {
	var result []netip.Prefix
	for _, value := range values {
		result = append(result, netip.MustParsePrefix(value))
	}
	return result
}

func TestClientIP(t *testing.T) {
	proxies := networks("172.17.0.0/16")
	testCases := []struct {
		name, remote, forwarded, want string
	}{
		{"direct", "203.0.113.7:5000", "", "203.0.113.7"},
		{"direct client cannot fake the header", "203.0.113.7:5000", "10.0.0.1", "203.0.113.7"},
		{"behind proxy", "172.17.0.1:5000", "198.51.100.4", "198.51.100.4"},
		{"spoofed entry left of the real client is ignored", "172.17.0.1:5000", "10.0.0.1, 198.51.100.4", "198.51.100.4"},
		{"chain of trusted proxies", "172.17.0.1:5000", "198.51.100.4, 172.17.0.9", "198.51.100.4"},
		{"proxy without header", "172.17.0.1:5000", "", "172.17.0.1"},
		{"malformed entry", "172.17.0.1:5000", "not-an-ip", "172.17.0.1"},
		{"ipv4-mapped ipv6", "[::ffff:203.0.113.7]:5000", "", "203.0.113.7"},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			request.RemoteAddr = testCase.remote
			if testCase.forwarded != "" {
				request.Header.Set("X-Forwarded-For", testCase.forwarded)
			}
			if got := ClientIP(request, proxies); got.String() != testCase.want {
				t.Fatalf("got %s, want %s", got, testCase.want)
			}
		})
	}
}

var ok = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })

func TestRequireAllowedIP(t *testing.T) {
	handler := RequireAllowedIP(networks("10.0.0.0/8"), networks("172.17.0.1/32"), ok)
	testCases := []struct {
		remote, forwarded string
		want              int
	}{
		{"10.1.2.3:1", "", http.StatusOK},
		{"203.0.113.7:1", "", http.StatusForbidden},
		{"203.0.113.7:1", "10.1.2.3", http.StatusForbidden}, // untrusted sender faking the header
		{"172.17.0.1:1", "10.1.2.3", http.StatusOK},
		{"172.17.0.1:1", "203.0.113.7", http.StatusForbidden},
	}
	for _, testCase := range testCases {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = testCase.remote
		if testCase.forwarded != "" {
			request.Header.Set("X-Forwarded-For", testCase.forwarded)
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != testCase.want {
			t.Errorf("%s via %q: got %d, want %d", testCase.forwarded, testCase.remote, response.Code, testCase.want)
		}
	}

	open := httptest.NewRecorder()
	RequireAllowedIP(nil, nil, ok).ServeHTTP(open, httptest.NewRequest(http.MethodGet, "/", nil))
	if open.Code != http.StatusOK {
		t.Fatal("an empty allowlist must allow everyone")
	}
}

func TestRequireToken(t *testing.T) {
	const token = "0123456789abcdef0123456789abcdef"
	testCases := []struct {
		name, configured, header string
		want                     int
	}{
		{"valid", token, "Bearer " + token, http.StatusOK},
		{"lowercase scheme", token, "bearer " + token, http.StatusOK},
		{"wrong token", token, "Bearer nope", http.StatusUnauthorized},
		{"token prefix only", token, "Bearer " + token[:10], http.StatusUnauthorized},
		{"missing", token, "", http.StatusUnauthorized},
		{"basic auth", token, "Basic " + token, http.StatusUnauthorized},
		{"api disabled", "", "Bearer ", http.StatusUnauthorized},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			request := httptest.NewRequest(http.MethodGet, "/", nil)
			if testCase.header != "" {
				request.Header.Set("Authorization", testCase.header)
			}
			response := httptest.NewRecorder()
			RequireToken(testCase.configured, ok).ServeHTTP(response, request)
			if response.Code != testCase.want {
				t.Fatalf("got %d, want %d", response.Code, testCase.want)
			}
		})
	}
}
