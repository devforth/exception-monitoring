package lists

import (
	"net/url"

	"github.com/devforth/exception-monitoring/internal/platform/config"
)

// Badge is a project or level badge, the one "badge" partial for both:
// a link when URL is set, a plain label otherwise (inside checkboxes and
// chips), with a × when RemoveURL is set (chips).
type Badge struct {
	Kind      string // "project", "level" or "status"
	Name      string
	URL       string
	RemoveURL string
}

// Label is a badge that is not a link (inside checkboxes): {{template "badge" (label "level" .Name)}}.
func Label(kind, name string) Badge {
	return Badge{Kind: kind, Name: name}
}

// ProjectBadge, LevelBadge and StatusBadge make badge links: within a list
// (its FilterLinks given) they filter that list, elsewhere they open the
// issues with that project, level (a log level named as the event level) or
// status.
func ProjectBadge(name string, links ...FilterLinks) Badge {
	return linkBadge("project", name, name, links)
}

func LevelBadge(level string, links ...FilterLinks) Badge {
	return linkBadge("level", level, config.EventLevel(level), links)
}

func StatusBadge(status string, links ...FilterLinks) Badge {
	return linkBadge("status", status, status, links)
}

// linkBadge: kind is also the query parameter; issueValue is the value for
// the issue list.
func linkBadge(kind, name, issueValue string, links []FilterLinks) Badge {
	badge := Badge{Kind: kind, Name: name, URL: "/issues?" + url.Values{kind: {issueValue}}.Encode()}

	if len(links) > 0 {
		badge.URL = links[0].With(kind, name)
	}

	return badge
}
