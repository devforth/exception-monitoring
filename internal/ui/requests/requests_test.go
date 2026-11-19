package requests

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"
)

func postForm(form url.Values) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	_ = request.ParseForm()

	return request
}

func TestReadBulkIssues(t *testing.T) {
	ids, problem := ReadBulkIssues(postForm(url.Values{"issue": {"issue003", "issue007"}}))

	if problem != "" || !slices.Equal(ids, []string{"issue003", "issue007"}) {
		t.Errorf("ReadBulkIssues = %v, %q", ids, problem)
	}

	if _, problem := ReadBulkIssues(postForm(url.Values{"issue": {"issue003", "x"}})); problem != "invalid issue id" {
		t.Errorf("bad id: problem = %q", problem)
	}

	many := make([]string, maxBulkIssues+1)

	for index := range many {
		many[index] = fmt.Sprint(index + 1)
	}

	if _, problem := ReadBulkIssues(postForm(url.Values{"issue": many})); problem == "" {
		t.Errorf("%d issues accepted, want the limit of %d", len(many), maxBulkIssues)
	}
}

func TestSafeNext(t *testing.T) {
	for next, want := range map[string]string{
		"/logs?level=error":    "/logs?level=error",
		"":                     "/issues",
		"https://example.com/": "/issues",
		"//example.com/":       "/issues",
		"/\\example.com":       "/issues",
		// Browsers drop tabs and newlines and read a backslash as a slash.
		"/\t/example.com":  "/issues",
		"/\n/example.com":  "/issues",
		"/\t\\example.com": "/issues",
		"/issues?q=a b":    "/issues",
		"/issues?q=a%20b":  "/issues?q=a%20b",
	} {
		if got := SafeNext(next); got != want {
			t.Errorf("SafeNext(%q) = %q, want %q", next, got, want)
		}
	}
}
