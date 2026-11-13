// Package routes is the route table of the UI: each pattern mapped to a
// controller, wrapped in the middleware it needs.
package routes

import (
	"net/http"

	"github.com/devforth/exception-monitoring/internal/ui/controllers"
	"github.com/devforth/exception-monitoring/internal/ui/middleware"
	"github.com/devforth/exception-monitoring/internal/ui/services"
	"github.com/devforth/exception-monitoring/internal/ui/views"
)

// Register adds the UI routes. protect is the IP allowlist.
func Register(router *http.ServeMux, protect func(http.Handler) http.Handler, controller *controllers.Controllers, auth *services.Auth) {
	requireLogin := middleware.RequireLogin(auth)
	sameOrigin := middleware.SameOrigin

	routes := map[string]http.HandlerFunc{
		"GET /{$}":                          requireLogin(controller.Home),
		"GET /login":                        controller.LoginPage,
		"POST /login":                       sameOrigin(controller.Login),
		"POST /logout":                      sameOrigin(controller.Logout),
		"GET /issues":                       requireLogin(controller.IssuesPage),
		"GET /issues/{issueID}":             requireLogin(controller.IssuePage),
		"POST /issues/{issueID}/action":     requireLogin(sameOrigin(controller.IssueAction)),
		"POST /issues/bulk":                 requireLogin(sameOrigin(controller.BulkIssueAction)),
		"GET /logs":                         requireLogin(controller.LogsPage),
		"GET /projects":                     requireLogin(controller.ProjectsPage),
		"GET /projects/new":                 requireLogin(controller.NewProjectPage),
		"POST /projects":                    requireLogin(sameOrigin(controller.CreateProject)),
		"GET /projects/{projectID}":         requireLogin(controller.ProjectPage),
		"POST /projects/{projectID}":        requireLogin(sameOrigin(controller.UpdateProject)),
		"POST /projects/{projectID}/delete": requireLogin(sameOrigin(controller.DeleteProject)),

		"GET /notifications":                        requireLogin(controller.ConnectionsPage),
		"GET /notifications/log":                    requireLogin(controller.DeliveriesPage),
		"GET /notifications/new":                    requireLogin(controller.NewConnectionPage),
		"POST /notifications":                       requireLogin(sameOrigin(controller.CreateConnection)),
		"GET /notifications/{connectionID}":         requireLogin(controller.ConnectionPage),
		"POST /notifications/{connectionID}":        requireLogin(sameOrigin(controller.UpdateConnection)),
		"POST /notifications/email/start":           requireLogin(sameOrigin(controller.EmailStart)),
		"POST /notifications/email/resend":          requireLogin(sameOrigin(controller.EmailResend)),
		"POST /notifications/email/confirm":         requireLogin(sameOrigin(controller.EmailConfirm)),
		"POST /notifications/email/change":          requireLogin(sameOrigin(controller.EmailChange)),
		"POST /notifications/{connectionID}/test":   requireLogin(sameOrigin(controller.TestConnection)),
		"POST /notifications/{connectionID}/active": requireLogin(sameOrigin(controller.SetConnectionActive)),
		"POST /notifications/{connectionID}/delete": requireLogin(sameOrigin(controller.DeleteConnection)),
		"GET /project-colors.css":                   requireLogin(controller.ProjectColors), // names projects
		"GET /static/":                              http.StripPrefix("/static/", middleware.StaticCache(http.FileServerFS(views.Static()))).ServeHTTP,
	}

	for pattern, route := range routes {
		router.Handle(pattern, protect(middleware.SecurityHeaders(route)))
	}
}
