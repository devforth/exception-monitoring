// Package fixtures gives tests the real SDK requests recorded in
// testdata/envelopes: <name>.envelope is the body, <name>.request.json the
// method, path, query and headers it was sent with. Only tests import it.
package fixtures

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/devforth/exception-monitoring/internal/platform/config"
)

// The public keys the fixtures were recorded with (the sentry-playground DSNs).
const (
	BackendKey = "backendkey0000000000000000000001"
	BrowserKey = "browserkey0000000000000000000002"
	// BackendEnvelopeURL is where tests send hand-made envelopes for project 1.
	BackendEnvelopeURL = "/api/1/envelope/?sentry_key=" + BackendKey
)

// Projects are the projects the recorded requests belong to: the Node SDK
// sent to project 1, the browser SDK to project 2.
func Projects() config.Projects {
	return config.Projects{
		{ID: 1, Name: "backend", PublicKey: BackendKey},
		{ID: 2, Name: "frontend", PublicKey: BrowserKey},
	}
}

// Request is one recorded SDK request.
type Request struct {
	Name    string
	Method  string            `json:"method"`
	Path    string            `json:"path"`
	Query   map[string]string `json:"query"`
	Headers map[string]string `json:"headers"`
	Body    []byte            `json:"-"`
}

// Browser reports whether the request came from the browser SDK.
func (request Request) Browser() bool {
	return strings.HasPrefix(request.Name, "browser-")
}

// HTTP builds the request as the SDK sent it, ready for handler.ServeHTTP.
func (request Request) HTTP() *http.Request {
	query := url.Values{}

	for key, value := range request.Query {
		query.Set(key, value)
	}

	built := httptest.NewRequest(request.Method, request.Path+"?"+query.Encode(), bytes.NewReader(request.Body))

	for key, value := range request.Headers {
		built.Header.Set(key, value)
	}

	return built
}

// dir is testdata/envelopes, found relative to this file so tests in any package can use it.
func dir() string {
	_, file, _, _ := runtime.Caller(0)

	return filepath.Join(filepath.Dir(file), "..", "..", "testdata", "envelopes")
}

// Envelope returns the body of one fixture.
func Envelope(t testing.TB, name string) []byte {
	t.Helper()

	body, err := os.ReadFile(filepath.Join(dir(), name+".envelope"))

	if err != nil {
		t.Fatal(err)
	}

	return body
}

// Load returns one fixture with its request metadata.
func Load(t testing.TB, name string) Request {
	t.Helper()

	meta, err := os.ReadFile(filepath.Join(dir(), name+".request.json"))

	if err != nil {
		t.Fatal(err)
	}

	request := Request{Name: name, Body: Envelope(t, name)}

	if err := json.Unmarshal(meta, &request); err != nil {
		t.Fatalf("%s.request.json: %v", name, err)
	}

	return request
}

// All returns every fixture, sorted by name.
func All(t testing.TB) []Request {
	t.Helper()

	files, err := filepath.Glob(filepath.Join(dir(), "*.request.json"))

	if err != nil || len(files) == 0 {
		t.Fatalf("no fixtures in %s: %v", dir(), err)
	}

	sort.Strings(files)

	requests := make([]Request, 0, len(files))

	for _, file := range files {
		requests = append(requests, Load(t, strings.TrimSuffix(filepath.Base(file), ".request.json")))
	}

	return requests
}
