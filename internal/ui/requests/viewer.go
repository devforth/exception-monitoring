package requests

import (
	"context"
	"net/http"

	"github.com/devforth/exception-monitoring/internal/store"
	"github.com/devforth/exception-monitoring/internal/ui/contracts"
)

type viewerKey struct{}

// WithViewer returns the request with its logged-in user (set by
// middleware.RequireLogin).
func WithViewer(r *http.Request, viewer store.User) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), viewerKey{}, viewer))
}

// Viewer is the logged-in user of the request; the zero User (no role)
// without a session.
func Viewer(r *http.Request) store.User {
	viewer, _ := r.Context().Value(viewerKey{}).(store.User)

	return viewer
}

// ReadUser reads the user form.
func ReadUser(r *http.Request) contracts.UserInput {
	return contracts.UserInput{Username: r.PostFormValue("username"), Name: r.PostFormValue("name"), Role: r.PostFormValue("role")}
}
