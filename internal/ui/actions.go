package ui

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"

	"github.com/devforth/exception-monitoring/internal/store"
)

// Issue actions are shared by the issue page (one issue) and the issues list
// (the selected issues): both post an "action" and end up in applyIssueAction.

// issueActions maps an action to the status it sets; delete sets none.
var issueActions = map[string]string{
	"resolve": store.StatusResolved,
	"ignore":  store.StatusIgnored,
	"reopen":  store.StatusUnresolved,
	"delete":  "",
}

var errUnknownAction = errors.New("unknown action")

// maxBulkIssues: one page of the largest size, with room to spare.
const maxBulkIssues = 1000

// applyIssueAction runs action on each issue. An issue that no longer exists
// (deleted from another tab) is skipped, the others are still processed.
func (handler *Handler) applyIssueAction(ctx context.Context, action string, issueIDs []int64) error {
	status, ok := issueActions[action]

	if !ok {
		return errUnknownAction
	}

	for _, issueID := range issueIDs {
		var err error

		if action == "delete" {
			err = handler.store.DeleteIssue(ctx, issueID)
		} else {
			err = handler.store.SetIssueStatus(ctx, issueID, status)
		}

		if err != nil && !errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("%s issue %d: %w", action, issueID, err)
		}
	}

	return nil
}

// issueAction handles POST /issues/{issueID}/action from the issue page.
func (handler *Handler) issueAction(w http.ResponseWriter, r *http.Request) {
	issue, ok := handler.loadIssue(w, r)

	if !ok {
		return
	}

	action := r.FormValue("action")

	if !handler.runAction(w, r, action, []int64{issue.ID}) {
		return
	}

	if action == "delete" {
		redirectAfterPost(w, r, "/issues")

		return
	}

	// Reload the page: the status shows up in the header and in the buttons.
	redirectAfterPost(w, r, fmt.Sprintf("/issues/%d", issue.ID))
}

// bulkIssueAction handles POST /issues/bulk from the issues list: "action",
// the selected "issue" IDs and "return", the list URL to go back to with its
// filters.
func (handler *Handler) bulkIssueAction(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)

		return
	}

	// The list shows at most 100 issues a page; a hand-made form with more
	// would only hold the database for a long time.
	if len(r.PostForm["issue"]) > maxBulkIssues {
		http.Error(w, fmt.Sprintf("at most %d issues at once", maxBulkIssues), http.StatusBadRequest)

		return
	}

	issueIDs := make([]int64, 0, len(r.PostForm["issue"]))

	for _, value := range r.PostForm["issue"] {
		issueID, err := strconv.ParseInt(value, 10, 64)

		if err != nil {
			http.Error(w, "invalid issue id", http.StatusBadRequest)

			return
		}

		issueIDs = append(issueIDs, issueID)
	}

	if !handler.runAction(w, r, r.PostFormValue("action"), issueIDs) {
		return
	}

	redirectAfterPost(w, r, safeNext(r.PostFormValue("return")))
}

// runAction applies the action and writes an error response if it fails.
func (handler *Handler) runAction(w http.ResponseWriter, r *http.Request, action string, issueIDs []int64) bool {
	err := handler.applyIssueAction(r.Context(), action, issueIDs)

	switch {
	case errors.Is(err, errUnknownAction):
		http.Error(w, "unknown action", http.StatusBadRequest)

		return false
	case err != nil:
		handler.serverError(w, r, err)

		return false
	}

	return true
}
