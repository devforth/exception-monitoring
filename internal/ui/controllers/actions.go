package controllers

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/devforth/exception-monitoring/internal/services"
	"github.com/devforth/exception-monitoring/internal/ui/requests"
)

// Issue actions are shared by the issue page (one issue) and the issues list
// (the selected issues): both post an "action" and end up in
// services.Issues.ApplyAction.

// IssueAction handles POST /issues/{issueID}/action from the issue page.
func (controller *Controllers) IssueAction(w http.ResponseWriter, r *http.Request) {
	issue, ok := controller.loadIssue(w, r)

	if !ok {
		return
	}

	action := r.FormValue("action")

	if !controller.runAction(w, r, action, []string{issue.ID}) {
		return
	}

	if action == "delete" {
		redirectAfterPost(w, r, "/issues")

		return
	}

	// Reload the page: the status shows up in the header and in the buttons.
	redirectAfterPost(w, r, fmt.Sprintf("/issues/%s", issue.ID))
}

// BulkIssueAction handles POST /issues/bulk from the issues list: "action",
// the selected "issue" IDs and "return", the list URL to go back to with its
// filters.
func (controller *Controllers) BulkIssueAction(w http.ResponseWriter, r *http.Request) {
	issueIDs, problem := requests.ReadBulkIssues(r)

	if problem != "" {
		http.Error(w, problem, http.StatusBadRequest)

		return
	}

	if !controller.runAction(w, r, r.PostFormValue("action"), issueIDs) {
		return
	}

	redirectAfterPost(w, r, requests.SafeNext(r.PostFormValue("return")))
}

// runAction applies the action and writes an error response if it fails.
func (controller *Controllers) runAction(w http.ResponseWriter, r *http.Request, action string, issueIDs []string) bool {
	err := controller.issues.ApplyAction(r.Context(), requests.Viewer(r), action, issueIDs)

	switch {
	case errors.Is(err, services.ErrUnknownAction):
		http.Error(w, "unknown action", http.StatusBadRequest)

		return false
	case errors.Is(err, services.ErrForbidden):
		controller.NotFoundPage(w, r)

		return false
	case err != nil:
		controller.serverError(w, r, err)

		return false
	}

	return true
}
