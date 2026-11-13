package lists

import (
	"net/url"
	"time"
)

// DateRange is the ?from= and ?to= filter: days as YYYY-MM-DD in UTC, both
// inclusive. An empty or invalid date means no bound.
type DateRange struct {
	From, To string // as typed, to fill the inputs back
	// Today (UTC) is the latest day the pickers offer: there are no events from the future.
	Today string
	// FromMax is the latest day "from" can be: "to" if set, else today.
	FromMax      string
	Since, Until time.Time
}

func ParseDateRange(query url.Values) DateRange {
	dates := DateRange{From: query.Get("from"), To: query.Get("to"), Today: time.Now().UTC().Format(time.DateOnly)}
	from, fromErr := time.Parse(time.DateOnly, dates.From)
	to, toErr := time.Parse(time.DateOnly, dates.To)

	// The date pickers do not allow it, but a typed or edited URL can put "to"
	// before "from". That range matches nothing, so treat it as the reverse.
	if fromErr == nil && toErr == nil && to.Before(from) {
		from, to = to, from
		dates.From, dates.To = dates.To, dates.From
	}

	if fromErr == nil {
		dates.Since = from
	}

	if toErr == nil {
		dates.Until = to.AddDate(0, 0, 1) // the whole "to" day is included
	}

	dates.FromMax = dates.Today

	if dates.To != "" && dates.To < dates.Today {
		dates.FromMax = dates.To
	}

	return dates
}
