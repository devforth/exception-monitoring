package lists

import (
	"net/url"
	"slices"
	"strconv"
	"strings"
)

// Pagination of the issue and log lists: ?page=N, ?per_page=20|50|100. Both
// stay in the URL, so a page can be shared and reloaded; changing a filter
// starts again from the first page (the filter form does not send "page").

var PerPageChoices = []int{20, 50, 100}

const defaultPerPage = 20

// ReadPaging returns the page (from 1) and the page size of the query.
func ReadPaging(query url.Values) (page, perPage int) {
	page, _ = strconv.Atoi(query.Get("page"))
	perPage, _ = strconv.Atoi(query.Get("per_page"))

	if !slices.Contains(PerPageChoices, perPage) {
		perPage = defaultPerPage
	}

	return max(page, 1), perPage
}

// Pager is the navigation under a list: "21–40 of 134", previous and next,
// and page numbers around the current one.
type Pager struct {
	Page, Pages    int
	Total          int
	First, Last    int // the rows shown, from 1
	PerPage        int
	PerPageChoices []int
	PrevURL        string
	NextURL        string
	Links          []PageLink
	Target         string // the element htmx replaces ("issue-list", "log-list")
	Form           string // the filter form the page size belongs to ("issue-filters", "log-filters")
}

type PageLink struct {
	Number  int
	URL     string
	Current bool
	Gap     bool // "…" between distant numbers
}

// pageCount is how many pages total rows make (at least one).
func pageCount(total, perPage int) int {
	return max(1, (total+perPage-1)/perPage)
}

func NewPager(links FilterLinks, page, perPage, total int, target string) Pager {
	pages := pageCount(total, perPage)
	page = min(page, pages)
	result := Pager{
		Page: page, Pages: pages, Total: total, PerPage: perPage, PerPageChoices: PerPageChoices, Target: target,
		Form:  strings.TrimSuffix(target, "-list") + "-filters",
		First: min(total, (page-1)*perPage+1), Last: min(total, page*perPage),
	}

	pageURL := func(number int) string {
		query := links.firstPage()

		if number > 1 {
			query.Set("page", strconv.Itoa(number))
		}

		return links.url(query)
	}

	if page > 1 {
		result.PrevURL = pageURL(page - 1)
	}

	if page < pages {
		result.NextURL = pageURL(page + 1)
	}

	// The first, the last, and two on each side of the current page.
	previous := 0

	for number := 1; number <= pages; number++ {
		if number != 1 && number != pages && (number < page-2 || number > page+2) {
			continue
		}

		if previous != 0 && number > previous+1 {
			result.Links = append(result.Links, PageLink{Gap: true})
		}

		result.Links = append(result.Links, PageLink{Number: number, URL: pageURL(number), Current: number == page})
		previous = number
	}

	return result
}
