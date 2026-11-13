// Package routes is the route table of the UI: each pattern mapped to a
// controller, wrapped in the middleware it needs.
package routes

import (
	"net/http"
	"strings"

	"github.com/devforth/exception-monitoring/internal/oauth"
	"github.com/devforth/exception-monitoring/internal/ui/controllers"
	"github.com/devforth/exception-monitoring/internal/ui/middleware"
	"github.com/devforth/exception-monitoring/internal/ui/services"
	"github.com/devforth/exception-monitoring/internal/ui/views"
)

// Register adds the UI routes. protect is the IP allowlist.
func Register(router *http.ServeMux, protect func(http.Handler) http.Handler, controller *controllers.Controllers, auth *services.Auth) {
	// Open to anyone who passes the IP allowlist: the login itself.
	public := group{
		"GET /login":   controller.LoginPage,
		"POST /login":  controller.Login,
		"POST /logout": controller.Logout,
	}

	// The dashboard: every route needs a login.
	dashboard := group{
		"GET /{$}":                          controller.Home,
		"GET /issues":                       controller.IssuesPage,
		"GET /issues/{issueID}":             controller.IssuePage,
		"POST /issues/{issueID}/action":     controller.IssueAction,
		"POST /issues/bulk":                 controller.BulkIssueAction,
		"GET /logs":                         controller.LogsPage,
		"GET /projects":                     controller.ProjectsPage,
		"GET /projects/new":                 controller.NewProjectPage,
		"POST /projects":                    controller.CreateProject,
		"GET /projects/{projectID}":         controller.ProjectPage,
		"POST /projects/{projectID}":        controller.UpdateProject,
		"POST /projects/{projectID}/delete": controller.DeleteProject,

		"GET /agents":                       controller.AgentsPage,
		"POST /agents/token":                controller.NewAgentToken,
		"POST /agents/{grantID}/disconnect": controller.DisconnectAgent,
		"GET " + oauth.AuthorizePath:        controller.AuthorizePage,
		"POST " + oauth.AuthorizePath:       controller.Authorize,

		"GET /notifications":                           controller.ConnectionsPage,
		"GET /notifications/log":                       controller.DeliveriesPage,
		"GET /notifications/new":                       controller.NewConnectionPage,
		"POST /notifications":                          controller.CreateConnection,
		"GET /notifications/{connectionID}":            controller.ConnectionPage,
		"POST /notifications/{connectionID}":           controller.UpdateConnection,
		"POST /notifications/email/start":              controller.EmailStart,
		"POST /notifications/email/resend":             controller.EmailResend,
		"POST /notifications/email/confirm":            controller.EmailConfirm,
		"POST /notifications/email/change":             controller.EmailChange,
		"POST /notifications/{connectionID}/test":      controller.TestConnection,
		"POST /notifications/test-chat":                controller.TestChat,
		"POST /notifications/{connectionID}/test-chat": controller.TestChat,
		"POST /notifications/{connectionID}/retry":     controller.RetryConnection,
		"POST /notifications/{connectionID}/active":    controller.SetConnectionActive,
		"POST /notifications/{connectionID}/delete":    controller.DeleteConnection,

		"GET /project-colors.css": controller.ProjectColors, // names the projects
	}

	public.register(router, protect)
	dashboard.register(router, protect, middleware.RequireLogin(auth))

	static := http.StripPrefix("/static/", middleware.StaticCache(http.FileServerFS(views.Static())))
	router.Handle("GET /static/", protect(middleware.SecurityHeaders(static)))
}

// group is a set of routes that share their middleware.
type group map[string]http.HandlerFunc

// register adds the routes, each wrapped in wrap (outermost first). Every
// POST also gets the same-origin check (CSRF), inside the others: a group
// cannot forget it.
func (routes group) register(router *http.ServeMux, protect func(http.Handler) http.Handler, wrap ...func(http.HandlerFunc) http.HandlerFunc) {
	for pattern, route := range routes {
		if strings.HasPrefix(pattern, http.MethodPost+" ") {
			route = middleware.SameOrigin(route)
		}

		for index := len(wrap) - 1; index >= 0; index-- {
			route = wrap[index](route)
		}

		router.Handle(pattern, protect(middleware.SecurityHeaders(route)))
	}
}
