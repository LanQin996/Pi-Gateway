// Package piwire reproduces the exact wire shape Pi uses on its openai-chatgpt
// route (api.openai.com/v1/responses): the request body layout, the header set,
// and the User-Agent string. The WebSocket header builder still mirrors Pi's
// websocket transport, which Pi implements on its codex provider.
//
// Every outbound request to the upstream is built from scratch here. Client
// headers are rebuilt; recognized Pi clients can supply allowlisted SDK headers.
package piwire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"sort"
	"strconv"
	"strings"
)

// Constants mirrored from Pi's api/openai-codex-responses.ts.
const (
	// BetaSSE is the OpenAI-Beta value for the SSE responses endpoint.
	BetaSSE = "responses=experimental"
	// BetaWebSocket is the OpenAI-Beta value for the WebSocket responses endpoint.
	BetaWebSocket = "responses_websockets=2026-02-06"

	// DefaultOriginator mirrors Pi's originator header.
	DefaultOriginator = "pi"

	// DefaultInstructions is Pi's fallback when no system prompt exists.
	DefaultInstructions = "You are a helpful assistant."

	// PromptCacheKeyMaxLength mirrors Pi's clampOpenAIPromptCacheKey.
	PromptCacheKeyMaxLength = 64

	// IncludeReasoningEncrypted mirrors Pi's include list.
	IncludeReasoningEncrypted = "reasoning.encrypted_content"

	// DefaultTextVerbosity mirrors Pi's default.
	DefaultTextVerbosity = "low"
)

const userAgentHeader = "User-Agent"

// DefaultUserAgent returns a plausible Pi User-Agent.
//
// Pi formats it as "pi (<platform> <release>; <arch>)" using Node's os module.
// We deliberately do not leak the real host platform: a Linux server value is both
// typical for Pi deployments and reveals nothing about this process.
func DefaultUserAgent() string {
	return "pi (linux 6.1.0; x64)"
}

// PlatformUserAgent builds a Pi User-Agent for the current host, for operators who
// prefer to expose their real platform.
func PlatformUserAgent() string {
	return fmt.Sprintf("pi (%s %s; %s)", nodePlatform(runtime.GOOS), releasePlaceholder(runtime.GOOS), nodeArch(runtime.GOARCH))
}

func nodePlatform(goos string) string {
	switch goos {
	case "windows":
		return "win32"
	case "darwin":
		return "darwin"
	default:
		return goos
	}
}

func nodeArch(goarch string) string {
	switch goarch {
	case "amd64":
		return "x64"
	case "386":
		return "ia32"
	default:
		return goarch
	}
}

func releasePlaceholder(goos string) string {
	switch goos {
	case "windows":
		return "10.0.22631"
	case "darwin":
		return "23.6.0"
	default:
		return "6.1.0"
	}
}

// ClampPromptCacheKey truncates a prompt cache key to Pi's 64 code points.
func ClampPromptCacheKey(key string) string {
	if key == "" {
		return ""
	}
	runes := []rune(key)
	if len(runes) <= PromptCacheKeyMaxLength {
		return key
	}
	return string(runes[:PromptCacheKeyMaxLength])
}

// Platform carries SDK-normalized platform metadata supplied by the caller.
// Empty fields are omitted from the SSE request headers.
type Platform struct {
	OS, Arch, Runtime, RuntimeVersion string
}

// HeaderOptions carries everything needed to build the upstream header set.
type HeaderOptions struct {
	// ClientHeaders contributes only Pi's SDK fingerprint, never credentials or Codex metadata.
	ClientHeaders http.Header
	AccessToken   string
	AccountID     string
	// SessionID is the wire session id; empty means no session headers are sent.
	SessionID  string
	Originator string
	UserAgent  string
	// TimeoutSeconds is sent only when positive; there is no header default.
	TimeoutSeconds int
	Platform       Platform
}

// BuildSSEHeaders builds the header set Pi sends to api.openai.com for the HTTP
// (SSE) Responses endpoint on the Sign in with ChatGPT route.
//
// This is the openai-responses shape (api/openai-responses.ts), using the pinned
// openai@7.19.0 SDK with Pi's User-Agent and optional session headers. It omits
// the Codex-only `originator`, `chatgpt-account-id` and `OpenAI-Beta` headers.
func BuildSSEHeaders(o HeaderOptions) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+o.AccessToken)
	h.Set(userAgentHeader, userAgentOr(o.UserAgent))
	h.Set("accept", "application/json")
	h.Set("content-type", "application/json")
	h.Set("X-Stainless-Lang", "js")
	h.Set("X-Stainless-Package-Version", "7.19.0")
	// Pi disables SDK retries and performs retries outside each SDK call.
	h.Set("X-Stainless-Retry-Count", "0")
	for name, value := range map[string]string{
		"X-Stainless-OS":              o.Platform.OS,
		"X-Stainless-Arch":            o.Platform.Arch,
		"X-Stainless-Runtime":         o.Platform.Runtime,
		"X-Stainless-Runtime-Version": o.Platform.RuntimeVersion,
	} {
		if value != "" {
			h.Set(name, value)
		}
	}
	if o.TimeoutSeconds > 0 {
		h.Set("X-Stainless-Timeout", strconv.Itoa(o.TimeoutSeconds))
	}
	if o.SessionID != "" {
		h.Set("session_id", o.SessionID)
		h.Set("x-client-request-id", o.SessionID)
	}
	// undici (Node's fetch, which Pi uses) advertises gzip+deflate by default.
	h.Set("accept-encoding", "gzip, deflate")
	if strings.HasPrefix(o.ClientHeaders.Get(userAgentHeader), "pi (") {
		for _, name := range []string{userAgentHeader, "Accept-Language", "X-Stainless-Lang", "X-Stainless-Package-Version", "X-Stainless-OS", "X-Stainless-Arch", "X-Stainless-Runtime", "X-Stainless-Runtime-Version", "X-Stainless-Timeout"} {
			value := o.ClientHeaders.Get(name)
			if value != "" && len(value) <= 512 && !strings.ContainsAny(value, "\r\n") {
				h.Set(name, value)
			}
		}
	}
	return h
}

// BuildWSHeaders builds the WebSocket handshake header set for the Responses
// socket.
//
// Pi deletes accept/content-type for the WebSocket transport and advertises the
// responses websockets beta. The `chatgpt-account-id` header is only meaningful on
// the Codex backend, so it is sent only when the credential actually carries an
// account id.
func BuildWSHeaders(o HeaderOptions) http.Header {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+o.AccessToken)
	if strings.TrimSpace(o.AccountID) != "" {
		h.Set("chatgpt-account-id", o.AccountID)
	}
	h.Set(userAgentHeader, userAgentOr(o.UserAgent))
	h.Set("OpenAI-Beta", BetaWebSocket)
	h.Set("originator", originatorOr(o.Originator))
	if o.SessionID != "" {
		h.Set("session-id", o.SessionID)
		h.Set("x-client-request-id", o.SessionID)
	}
	return h
}

func originatorOr(v string) string {
	if strings.TrimSpace(v) == "" {
		return DefaultOriginator
	}
	return v
}

func userAgentOr(v string) string {
	if strings.TrimSpace(v) == "" {
		return DefaultUserAgent()
	}
	return v
}

// OrderedMap marshals to JSON preserving insertion order, which keeps our request
// bytes in the same key order Pi produces with JSON.stringify.
type OrderedMap struct {
	keys   []string
	values map[string]any
}

// NewOrderedMap creates an empty ordered map.
func NewOrderedMap() *OrderedMap {
	return &OrderedMap{values: map[string]any{}}
}

// Set appends or replaces a key, keeping first-insertion position (matching
// JavaScript object semantics).
func (m *OrderedMap) Set(key string, value any) *OrderedMap {
	if _, exists := m.values[key]; !exists {
		m.keys = append(m.keys, key)
	}
	m.values[key] = value
	return m
}

// SetLast moves the key to the end of the order (used for previous_response_id,
// which JS appends because the spread already copied input earlier).
func (m *OrderedMap) SetLast(key string, value any) *OrderedMap {
	m.Delete(key)
	m.keys = append(m.keys, key)
	m.values[key] = value
	return m
}

// Delete removes a key.
func (m *OrderedMap) Delete(key string) *OrderedMap {
	if _, exists := m.values[key]; !exists {
		return m
	}
	delete(m.values, key)
	for i, k := range m.keys {
		if k == key {
			m.keys = append(m.keys[:i], m.keys[i+1:]...)
			break
		}
	}
	return m
}

// Has reports whether a key is present.
func (m *OrderedMap) Has(key string) bool {
	_, ok := m.values[key]
	return ok
}

// Get returns a value.
func (m *OrderedMap) Get(key string) (any, bool) {
	v, ok := m.values[key]
	return v, ok
}

// Keys returns the ordered key list.
func (m *OrderedMap) Keys() []string { return append([]string(nil), m.keys...) }

// MarshalJSON renders the map in insertion order.
func (m *OrderedMap) MarshalJSON() ([]byte, error) {
	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range m.keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := marshalValue(m.values[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}

// marshalValue encodes a value, sorting plain maps so output is deterministic.
func marshalValue(v any) ([]byte, error) {
	switch t := v.(type) {
	case *OrderedMap:
		return t.MarshalJSON()
	case map[string]any:
		return marshalSortedMap(t)
	default:
		return json.Marshal(v)
	}
}

func marshalSortedMap(m map[string]any) ([]byte, error) {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var buf bytes.Buffer
	buf.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			buf.WriteByte(',')
		}
		kb, err := json.Marshal(k)
		if err != nil {
			return nil, err
		}
		buf.Write(kb)
		buf.WriteByte(':')
		vb, err := marshalValue(m[k])
		if err != nil {
			return nil, err
		}
		buf.Write(vb)
	}
	buf.WriteByte('}')
	return buf.Bytes(), nil
}
