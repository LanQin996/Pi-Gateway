package admin

import (
	"encoding/json"
	"errors"
	"html"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"pi-gateway/internal/store"
	"pi-gateway/internal/upstream"
)

const maxModelTestDiagnostic = 16 << 10
const modelTestRedacted = "[REDACTED]"

var (
	modelTestJSONString    = regexp.MustCompile(`"(?:\\.|[^"\\])*"`)
	modelTestBearer        = regexp.MustCompile(`(?i)\bbearer\s+[^\s"'<>\\,;\}\]]+`)
	modelTestProxyPassword = regexp.MustCompile(`(?i)([a-z][a-z0-9+.-]*://[^\s/@:]+:)[^\s/@]+@`)
	modelTestAuthField     = regexp.MustCompile(`(?i)(\b(?:proxy[-_]?)?authorization["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*(?:"|$)|'[^']*(?:'|$)|[^\r\n,;}<]+)`)
	modelTestSecretField   = regexp.MustCompile(`(?is)(\b[a-z0-9_.-]*(?:token|password|passwd|authorization|api[_-]?key|secret|credential|cookie)[a-z0-9_.-]*["']?\s*[:=]\s*)(?:"(?:\\.|[^"\\])*(?:"|$)|'(?:\\.|[^'\\])*(?:'|$)|[\{\[].*|[^\r\n,;<>}\]]+)`)
)

// modelTestRedactor is request-local and knows both credential snapshots and the
// fresh token actually sent. Capture's header masker intentionally preserves
// token prefixes/suffixes and therefore is not suitable for these diagnostics.
type modelTestRedactor struct{ secrets []string }

func (r *modelTestRedactor) addAccount(a *store.Account) {
	if a == nil {
		return
	}
	for _, value := range []string{a.AccessToken, a.RefreshToken, a.IDToken, a.CodexAccessToken, a.CodexRefreshToken, a.CodexIDToken} {
		r.add(value)
	}
	if proxy, err := url.Parse(a.ProxyURL); err == nil && proxy.User != nil {
		if password, ok := proxy.User.Password(); ok {
			r.add(password)
		}
	}
}

func (r *modelTestRedactor) add(secret string) {
	if secret == "" {
		return
	}
	encoded, _ := json.Marshal(secret)
	for _, value := range []string{secret, string(encoded[1 : len(encoded)-1]), url.QueryEscape(secret), url.PathEscape(secret), html.EscapeString(secret)} {
		if value == "" {
			continue
		}
		found := false
		for _, old := range r.secrets {
			if old == value {
				found = true
				break
			}
		}
		if !found {
			r.secrets = append(r.secrets, value)
		}
	}
	// Avoid leaking the suffix of a longer refresh token sharing an access prefix.
	sort.Slice(r.secrets, func(i, j int) bool { return len(r.secrets[i]) > len(r.secrets[j]) })
}

func modelTestSensitiveKey(key string) bool {
	key = strings.ToLower(strings.Map(func(c rune) rune {
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			return c
		}
		return -1
	}, key))
	for _, part := range []string{"token", "password", "passwd", "authorization", "apikey", "secret", "credential", "cookie"} {
		if strings.Contains(key, part) {
			return true
		}
	}
	return false
}

func (r *modelTestRedactor) replaceSecrets(text string) string {
	for _, secret := range r.secrets {
		text = strings.ReplaceAll(text, secret, modelTestRedacted)
	}
	return text
}

func (r *modelTestRedactor) scrub(text string, depth int) string {
	if text == "" {
		return ""
	}
	// Nested serialized JSON is untrusted. Fail closed rather than leaving a
	// deeply escaped credential beyond our recursive decoding budget.
	if depth > 32 {
		return modelTestRedacted
	}
	if encoded, ok := r.scrubJSONDocument(text, depth); ok {
		return encoded
	}
	// A proxy can embed JSON-escaped text without the outer JSON string
	// quotes. Decode one layer for inspection, then preserve that wire shape.
	if strings.Contains(text, `\`) {
		var decoded string
		if json.Unmarshal([]byte(`"`+text+`"`), &decoded) == nil && decoded != text {
			encoded, _ := json.Marshal(r.scrub(decoded, depth+1))
			return string(encoded[1 : len(encoded)-1])
		}
	}
	// Decode JSON string literals even inside plaintext / a truncated JSON
	// object. This handles escaped field names and embedded serialized JSON.
	text = modelTestJSONString.ReplaceAllStringFunc(text, func(literal string) string {
		var decoded string
		if json.Unmarshal([]byte(literal), &decoded) != nil {
			return literal
		}
		encoded, _ := json.Marshal(r.scrub(decoded, depth+1))
		return string(encoded)
	})
	text = r.replaceSecrets(text)
	text = modelTestBearer.ReplaceAllString(text, "Bearer "+modelTestRedacted)
	text = modelTestProxyPassword.ReplaceAllString(text, "${1}"+modelTestRedacted+"@")
	text = modelTestAuthField.ReplaceAllString(text, "${1}"+modelTestRedacted)
	return modelTestSecretField.ReplaceAllString(text, "${1}"+modelTestRedacted)
}

func (r *modelTestRedactor) scrubValue(value any, depth int) any {
	if depth > 32 {
		return modelTestRedacted
	}
	switch v := value.(type) {
	case map[string]any:
		clean := make(map[string]any, len(v))
		for key, item := range v {
			name := r.replaceSecrets(key)
			if modelTestSensitiveKey(key) {
				clean[name] = modelTestRedacted
			} else {
				clean[name] = r.scrubValue(item, depth+1)
			}
		}
		return clean
	case []any:
		for i, item := range v {
			v[i] = r.scrubValue(item, depth+1)
		}
		return v
	case string:
		return r.scrub(v, depth+1)
	default:
		return value
	}
}

// bounded always redacts before its UTF-8-safe output cut. If an upstream read
// limit already clipped the source, remove any known secret's incomplete suffix
// too; otherwise redaction-induced shrinking could expose that boundary fragment.
func (r *modelTestRedactor) bounded(text string, limit int, sourceTruncated bool) (string, bool) {
	if sourceTruncated {
		// A clipped JSON string may end in a partial Unicode escape. Its
		// decoded credential cannot safely be matched, so omit that unfinished
		// literal rather than return any undecodable secret fragment.
		text = redactUnfinishedLiteral(text)
		text = r.redactPartialSecret(text)
	}
	text = strings.ToValidUTF8(r.scrub(text, 0), "\uFFFD")
	truncated := sourceTruncated || len(text) > limit
	if len(text) > limit {
		text = text[:limit]
		for !utf8.ValidString(text) {
			text = text[:len(text)-1]
		}
	}
	return text, truncated
}

func modelTestDiagnosticError(err error, safeBody string) string {
	// Preserve cancellation, timeout and local safety-limit classifications.
	if errors.Is(err, errModelTestOutput) || errors.Is(err, errModelTestWire) || upstream.IsIdleTimeoutError(err) {
		return modelTestSafeError(err)
	}
	var value any
	if json.Unmarshal([]byte(safeBody), &value) == nil {
		if message := modelTestErrorMessage(value); message != "" {
			return message
		}
	}
	if err != nil {
		return err.Error()
	}
	return modelTestSafeError(err)
}

func modelTestErrorMessage(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case map[string]any:
		for _, key := range []string{"error", "response", "incomplete_details", "message", "reason"} {
			if message := modelTestErrorMessage(v[key]); message != "" {
				return message
			}
		}
	}
	return ""
}

func redactUnfinishedLiteral(text string) string {
	quotedAt, escaped := -1, false
	for i, c := range text {
		if escaped {
			escaped = false
			continue
		}
		if c == '\\' {
			escaped = true
			continue
		}
		if c == '"' {
			if quotedAt < 0 {
				quotedAt = i
			} else {
				quotedAt = -1
			}
		}
	}
	if quotedAt >= 0 {
		text = text[:quotedAt] + `"` + modelTestRedacted + `"`
	}
	return text
}

func (r *modelTestRedactor) redactPartialSecret(text string) string {
	longest := 0
	for _, secret := range r.secrets {
		for n := 1; n < len(secret) && n <= len(text); n++ {
			if n > longest && strings.HasSuffix(text, secret[:n]) {
				longest = n
			}
		}
	}
	if longest > 0 {
		text = text[:len(text)-longest] + modelTestRedacted
	}
	return text
}

func (r *modelTestRedactor) scrubJSONDocument(text string, depth int) (string, bool) {
	trimmed := strings.TrimSpace(text)
	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") || strings.HasPrefix(trimmed, `"`) {
		var value any
		if json.Valid([]byte(text)) {
			decoder := json.NewDecoder(strings.NewReader(text))
			decoder.UseNumber()
			if decoder.Decode(&value) == nil {
				value = r.scrubValue(value, depth+1)
				encoded, err := json.MarshalIndent(value, "", "  ")
				if err == nil {
					return string(encoded), true
				}
			}
		}
	}
	return "", false
}
