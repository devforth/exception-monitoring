package ingest

import (
	"compress/gzip"
	"compress/zlib"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// MaxBodyBytes caps an envelope both before and after decompression.
// Events are usually a few KB; large ones carry local variables or breadcrumbs.
const MaxBodyBytes = 20 << 20

// readBody reads the request body, decompressing gzip or deflate.
// SDKs don't always send Content-Type (Node sends none, the browser sends
// text/plain to avoid a CORS preflight), so it is ignored.
func readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	var reader io.Reader = http.MaxBytesReader(w, r.Body, MaxBodyBytes)

	switch encoding := strings.ToLower(strings.TrimSpace(r.Header.Get("Content-Encoding"))); encoding {
	case "", "identity":
	case "gzip", "x-gzip":
		decompressor, err := gzip.NewReader(reader)
		if err != nil {
			return nil, readError(err)
		}
		defer decompressor.Close()
		reader = decompressor
	case "deflate":
		decompressor, err := zlib.NewReader(reader)
		if err != nil {
			return nil, readError(err)
		}
		defer decompressor.Close()
		reader = decompressor
	default:
		return nil, reject(http.StatusUnsupportedMediaType, "unsupported Content-Encoding %q (use gzip, deflate or none)", encoding)
	}

	// +1 byte to tell "exactly at the limit" from "over the limit" after decompression.
	body, err := io.ReadAll(io.LimitReader(reader, MaxBodyBytes+1))
	if err != nil {
		return nil, readError(err)
	}
	if len(body) > MaxBodyBytes {
		return nil, reject(http.StatusRequestEntityTooLarge, "envelope is larger than %d bytes", MaxBodyBytes)
	}
	return body, nil
}

func readError(err error) error {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		return reject(http.StatusRequestEntityTooLarge, "envelope is larger than %d bytes", MaxBodyBytes)
	}
	return reject(http.StatusBadRequest, "read body: %v", err)
}

// publicKeyFromRequest returns the key from the "sentry_key" query parameter
// (JS SDKs) or from "X-Sentry-Auth: Sentry sentry_key=..., sentry_version=7"
// (most other SDKs; some send the same value in Authorization).
func publicKeyFromRequest(r *http.Request) string {
	if key := r.URL.Query().Get("sentry_key"); key != "" {
		return key
	}
	for _, header := range []string{"X-Sentry-Auth", "Authorization"} {
		value := strings.TrimSpace(r.Header.Get(header))
		if len(value) < len("Sentry ") || !strings.EqualFold(value[:len("Sentry ")], "Sentry ") {
			continue
		}
		for _, pair := range strings.Split(value[len("Sentry "):], ",") {
			name, keyValue, found := strings.Cut(strings.TrimSpace(pair), "=")
			if found && strings.TrimSpace(name) == "sentry_key" {
				return strings.TrimSpace(keyValue)
			}
		}
	}
	return ""
}

// publicKeyFromDSN returns the key from a DSN in the envelope header
// ("https://<key>@host/<projectID>"), which SDKs add when sending through a
// tunnel. The DSN must point at the same project as the request path.
func publicKeyFromDSN(dsn string, projectID int) string {
	if dsn == "" {
		return ""
	}
	parsed, err := url.Parse(dsn)
	if err != nil || parsed.User == nil {
		return ""
	}
	path := strings.TrimSuffix(parsed.Path, "/")
	if path[strings.LastIndexByte(path, '/')+1:] != strconv.Itoa(projectID) {
		return ""
	}
	return parsed.User.Username()
}
