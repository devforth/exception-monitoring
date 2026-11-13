package requests

import (
	"fmt"
	"net/http"
	"strconv"
)

// maxBulkIssues: one page of the largest size, with room to spare.
const maxBulkIssues = 1000

// ReadBulkIssues reads the selected "issue" IDs of the issues list. problem
// is the reason the form is rejected (400), empty when it is valid.
func ReadBulkIssues(r *http.Request) (issueIDs []int64, problem string) {
	if err := r.ParseForm(); err != nil {
		return nil, "invalid form"
	}

	// The list shows at most 100 issues a page; a hand-made form with more
	// would only hold the database for a long time.
	if len(r.PostForm["issue"]) > maxBulkIssues {
		return nil, fmt.Sprintf("at most %d issues at once", maxBulkIssues)
	}

	issueIDs = make([]int64, 0, len(r.PostForm["issue"]))

	for _, value := range r.PostForm["issue"] {
		issueID, err := strconv.ParseInt(value, 10, 64)

		if err != nil {
			return nil, "invalid issue id"
		}

		issueIDs = append(issueIDs, issueID)
	}

	return issueIDs, ""
}
