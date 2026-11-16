// Package httpjson writes JSON HTTP responses.
package httpjson

import (
	"encoding/json"
	"net/http"
)

// Write sends body as JSON with the given status.
func Write(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	// Responses carry error data: no cache may keep them.
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body) // the client may be gone; nothing useful to do then
}

// Error sends {"error": message}, the error shape of the REST API.
func Error(w http.ResponseWriter, status int, message string) {
	Write(w, status, map[string]string{"error": message})
}
