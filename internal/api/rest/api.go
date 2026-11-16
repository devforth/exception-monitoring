// Package rest serves the request catalog (package catalog) as a REST API under
// /api/v1/, behind the API token and the IP allowlist. Each endpoint is
// /api/v1/<name>: a GET with the input as query parameters when it only
// reads (?project=backend&limit=50; a list field takes repeated or
// comma-separated values), a POST with the input as a JSON object otherwise.
// Agents get the same endpoints as MCP tools, so both always match.
//
// Lists return {"items": [...], "next_cursor": "..."}; pass next_cursor as
// cursor to get the next page. No next_cursor means the last page.
package rest

import (
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"

	"github.com/devforth/exception-monitoring/internal/api/catalog"
	"github.com/devforth/exception-monitoring/internal/server/access"
	"github.com/devforth/exception-monitoring/internal/server/httpjson"
	"github.com/devforth/exception-monitoring/internal/storage/store"
)

// Prefix is where the endpoints are served.
const Prefix = "/api/v1/"

// maxBody caps a POST body: inputs are a few fields.
const maxBody = 1 << 16

type Handler struct {
	logger         *slog.Logger
	endpoints      []catalog.Endpoint
	publicURL      string
	trustedProxies []netip.Prefix
}

// New serves the catalog; publicURL and trustedProxies give the server's
// address for DSNs, as on the Projects page.
func New(logger *slog.Logger, endpoints []catalog.Endpoint, publicURL string, trustedProxies []netip.Prefix) *Handler {
	return &Handler{logger: logger, endpoints: endpoints, publicURL: publicURL, trustedProxies: trustedProxies}
}

// Route is the method and path of an endpoint.
func Route(endpoint catalog.Endpoint) string {
	method := http.MethodPost

	if endpoint.ReadOnly {
		method = http.MethodGet
	}

	return method + " " + Prefix + endpoint.Name
}

// Register adds the routes to router, each wrapped with protect (IP
// allowlist and token).
func (handler *Handler) Register(router *http.ServeMux, protect func(http.Handler) http.Handler) {
	for _, endpoint := range handler.endpoints {
		// Agent-only endpoints act as a user; the token is none.
		if !endpoint.AgentsOnly {
			router.Handle(Route(endpoint), protect(handler.serve(endpoint)))
		}
	}

	// Any other /api/ address: a JSON 404, not the UI's page. Less specific
	// than the ingest route /api/{projectID}/envelope/, so it does not overlap it.
	router.Handle("/api/", protect(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		httpjson.Error(w, http.StatusNotFound, "unknown API endpoint: see /api/v1/<name> in docs/api.md")
	})))
}

// apiActor is who changes a status through the REST API: its one token.
var apiActor = store.Actor{Name: "API token", Source: store.SourceAPI}

func (handler *Handler) serve(endpoint catalog.Endpoint) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		input, err := readInput(endpoint, w, r)

		if err != nil {
			httpjson.Error(w, http.StatusBadRequest, err.Error())
			return
		}

		call := catalog.Call{ByToken: true, Actor: apiActor, Address: access.PublicURL(r, handler.publicURL, handler.trustedProxies)}
		output, err := endpoint.Run(r.Context(), call, input)

		if err != nil {
			handler.fail(w, r, endpoint, err)
			return
		}

		if text, ok := output.(catalog.Text); ok {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = io.WriteString(w, string(text)) // the client went away; nothing to do

			return
		}

		httpjson.Write(w, http.StatusOK, output)
	})
}

func (handler *Handler) fail(w http.ResponseWriter, r *http.Request, endpoint catalog.Endpoint, err error) {
	message, notFound, ok := catalog.Outcome(err)

	switch {
	case !ok:
		handler.logger.Error("api request failed", "endpoint", endpoint.Name, "path", r.URL.Path, "err", err)
		httpjson.Error(w, http.StatusInternalServerError, "internal error")
	case notFound:
		httpjson.Error(w, http.StatusNotFound, message)
	default:
		httpjson.Error(w, http.StatusBadRequest, message)
	}
}

// readInput is the query (GET) or the JSON body (POST) as the endpoint's
// input object; the endpoint validates it against its schema.
func readInput(endpoint catalog.Endpoint, w http.ResponseWriter, r *http.Request) (map[string]any, error) {
	if r.Method == http.MethodGet {
		return queryInput(endpoint, r)
	}

	if r.URL.RawQuery != "" {
		return nil, fmt.Errorf("pass the input of %s as a JSON body", endpoint.Name)
	}

	input := map[string]any{}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBody))

	if err != nil {
		return nil, fmt.Errorf("read body: %w", err)
	}

	if len(strings.TrimSpace(string(body))) == 0 {
		return input, nil
	}

	if err := json.Unmarshal(body, &input); err != nil {
		return nil, fmt.Errorf("the body must be a JSON object: %w", err)
	}

	return input, nil
}

// queryInput converts query strings to the types of the input fields.
func queryInput(endpoint catalog.Endpoint, r *http.Request) (map[string]any, error) {
	input := map[string]any{}

	for name, values := range r.URL.Query() {
		fieldType, itemType, ok := endpoint.FieldType(name)

		if !ok {
			return nil, fmt.Errorf("unknown parameter %q", name)
		}

		if fieldType == "array" {
			var items []any

			for _, value := range values {
				for _, item := range strings.Split(value, ",") {
					converted, err := convert(name, itemType, strings.TrimSpace(item))

					if err != nil {
						return nil, err
					}

					items = append(items, converted)
				}
			}

			input[name] = items

			continue
		}

		if len(values) > 1 {
			return nil, fmt.Errorf("parameter %q given more than once", name)
		}

		converted, err := convert(name, fieldType, values[0])

		if err != nil {
			return nil, err
		}

		input[name] = converted
	}

	return input, nil
}

func convert(name, fieldType, value string) (any, error) {
	switch fieldType {
	case "integer":
		number, err := strconv.ParseInt(value, 10, 64)

		if err != nil {
			return nil, fmt.Errorf("%s must be an integer", name)
		}

		return number, nil
	case "boolean":
		flag, err := strconv.ParseBool(value)

		if err != nil {
			return nil, fmt.Errorf("%s must be true or false", name)
		}

		return flag, nil
	default:
		return value, nil
	}
}
