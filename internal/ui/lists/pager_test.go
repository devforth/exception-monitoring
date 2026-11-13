package lists

import (
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"
)

func TestReadPaging(t *testing.T) {
	for _, test := range []struct {
		query         url.Values
		page, perPage int
	}{
		{url.Values{}, 1, 20},
		{url.Values{"page": {"3"}, "per_page": {"50"}}, 3, 50},
		{url.Values{"page": {"-2"}, "per_page": {"7"}}, 1, 20},
	} {
		page, perPage := ReadPaging(test.query)

		if page != test.page || perPage != test.perPage {
			t.Errorf("ReadPaging(%v) = %d, %d, want %d, %d", test.query, page, perPage, test.page, test.perPage)
		}
	}
}

func testLinks(query url.Values) FilterLinks {
	return NewFilterLinks(httptest.NewRequest("GET", "/issues", nil), query)
}

func TestPager(t *testing.T) {
	links := testLinks(url.Values{"level": {"error"}, "page": {"5"}})
	pager := NewPager(links, 5, 20, 134, "issue-list")

	if pager.Pages != 7 || pager.First != 81 || pager.Last != 100 {
		t.Errorf("pager = %d pages, rows %d–%d, want 7 pages, rows 81–100", pager.Pages, pager.First, pager.Last)
	}

	if pager.PrevURL != "/issues?level=error&page=4" || pager.NextURL != "/issues?level=error&page=6" {
		t.Errorf("prev %q, next %q", pager.PrevURL, pager.NextURL)
	}

	// 1 … 3 4 [5] 6 7: a gap between 1 and 3, the first page without "page".
	var numbers []int

	for _, link := range pager.Links {
		numbers = append(numbers, link.Number) // a gap is 0
	}

	if want := []int{1, 0, 3, 4, 5, 6, 7}; !slices.Equal(numbers, want) {
		t.Errorf("page links = %v, want %v", numbers, want)
	}

	if pager.Links[0].URL != "/issues?level=error" {
		t.Errorf("first page URL = %q, want no page parameter", pager.Links[0].URL)
	}

	// Past the end: the last page.
	past := NewPager(links, 40, 20, 134, "issue-list")

	if past.Page != 7 || past.NextURL != "" || past.First != 121 || past.Last != 134 {
		t.Errorf("past the end = page %d, rows %d–%d, next %q", past.Page, past.First, past.Last, past.NextURL)
	}

	empty := NewPager(links, 1, 20, 0, "issue-list")

	if empty.Pages != 1 || empty.First != 0 || empty.PrevURL != "" || empty.NextURL != "" {
		t.Errorf("empty list pager = %+v", empty)
	}
}
