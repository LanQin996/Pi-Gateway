package rules

import (
	"fmt"
	"net/http"
	"strings"
)

// HeaderDocument preserves multiple values and canonicalizes names once at the
// boundary, so ordinary pointer operations have deterministic case-insensitive keys.
func HeaderDocument(h http.Header) map[string]any {
	out := map[string]any{}
	for name, values := range h {
		key := strings.ToLower(name)
		arr, _ := out[key].([]any)
		for _, v := range values {
			arr = append(arr, v)
		}
		out[key] = arr
	}
	return out
}
func DecodeHeaders(v any) (http.Header, error) {
	m, ok := object(v)
	if !ok {
		return nil, fmt.Errorf("headers must be an object")
	}
	out := http.Header{}
	for name, value := range m {
		if err := validateHeaderName(name); err != nil {
			return nil, err
		}
		key := http.CanonicalHeaderKey(name)
		if _, duplicate := out[key]; duplicate {
			return nil, fmt.Errorf("duplicate header name %q", name)
		}
		values, err := decodeHeaderValues(name, value)
		if err != nil {
			return nil, err
		}
		out[key] = values

	}
	return out, nil
}

// RedactedHeaders supplies client facts without making bearer tokens available
// to expressions or simulation checkpoints.
func RedactedHeaders(h http.Header) map[string]any {
	out := HeaderDocument(h)
	for k := range out {
		if sensitiveKey(k) {
			delete(out, k)
		}
	}
	return out
}

func validateHeaderName(name string) error {
	if name == "" {
		return fmt.Errorf("empty header name")
	}
	for _, c := range name {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("!#$%&'*+-.^_`|~", c)) {
			return fmt.Errorf("invalid header name %q", name)
		}
	}
	if sensitiveKey(name) {
		return fmt.Errorf("credential header %q is owned by the gateway", name)
	}
	if contains([]string{"connection", "upgrade", "host", "content-length", "transfer-encoding", "sec-websocket-key", "sec-websocket-version", "sec-websocket-extensions"}, strings.ToLower(name)) {
		return fmt.Errorf("transport header %q is owned by the transport", name)
	}

	return nil
}

func decodeHeaderValues(name string, value any) ([]string, error) {
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("header %q must be an array of strings", name)
	}
	out := []string{}
	for _, item := range values {
		text, ok := item.(string)
		if !ok || strings.ContainsFunc(text, func(c rune) bool { return c < 32 && c != '\t' || c == 127 }) {
			return nil, fmt.Errorf("invalid value for header %q", name)
		}
		out = append(out, text)
	}
	return out, nil
}
