package controllers

import "net/http"

// ProjectColors serves the colors of project badges. Inline styles are
// blocked by the CSP, so colors chosen in the UI come as a stylesheet; its
// URL carries a version of the content, so it can be cached for good.
func (controller *Controllers) ProjectColors(w http.ResponseWriter, r *http.Request) {
	colors := controller.registry.Colors()

	w.Header().Set("Content-Type", "text/css; charset=utf-8")

	if r.URL.Query().Get("v") == colors.Version {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "no-cache")
	}

	_, _ = w.Write([]byte(colors.CSS))
}
