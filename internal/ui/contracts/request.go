package contracts

import "encoding/json"

// RequestInfo is the "request" context of an event, as the issue page shows it.
type RequestInfo struct {
	Method  string            `json:"method"`
	URL     string            `json:"url"`
	Headers map[string]string `json:"headers"`
}

// ParseRequest decodes the event's "request" context. Headers may come as an
// object or as [name, value] pairs; pairs are converted.
func ParseRequest(raw json.RawMessage) *RequestInfo {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}

	var info RequestInfo

	if err := json.Unmarshal(raw, &info); err != nil {
		var loose struct {
			Method  string      `json:"method"`
			URL     string      `json:"url"`
			Headers [][2]string `json:"headers"`
		}

		if json.Unmarshal(raw, &loose) != nil {
			return nil
		}

		info = RequestInfo{Method: loose.Method, URL: loose.URL, Headers: map[string]string{}}

		for _, pair := range loose.Headers {
			info.Headers[pair[0]] = pair[1]
		}
	}

	return &info
}
