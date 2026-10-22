// Package codexbridge adapts Codex's native collaboration protocol to an ordinary
// Responses namespace. It is deliberately independent of transport and session state.
package codexbridge

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

const (
	Alias     = "pi_codex_collaboration_v1"
	Namespace = "collaboration"
)

// Tool bits are stable wire metadata. Do not reorder or reuse them.
const (
	ToolSpawnAgent uint32 = 1 << iota
	ToolSendMessage
	ToolFollowupTask
	ToolWaitAgent
	ToolListAgents
	ToolInterruptAgent
	knownTools   = ToolSpawnAgent | ToolSendMessage | ToolFollowupTask | ToolWaitAgent | ToolListAgents | ToolInterruptAgent
	messageTools = ToolSpawnAgent | ToolSendMessage | ToolFollowupTask
)

// Profile is bounded continuation metadata; its zero value disables adaptation.
type Profile struct {
	Version int    `json:"version,omitempty"`
	Tools   uint32 `json:"tools,omitempty"`
}

func (p Profile) Enabled() bool { return p.Version == 1 && p.Valid() }
func (p Profile) Valid() bool {
	return (p.Version == 0 && p.Tools == 0) || (p.Version == 1 && p.Tools & ^uint32(knownTools) == 0)
}

// Error never includes request contents, tool arguments, or encrypted payloads.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return e.Code + ": " + e.Message }
func protocolError(code, path, message string) error {
	return &Error{Code: code, Message: path + ": " + message}
}
func validProfile(p Profile) error {
	if !p.Valid() {
		return protocolError("codex_collaboration_profile", "profile", "unsupported collaboration profile")
	}
	return nil
}

// Merge validates both profiles before combining their registered tool bits.
func Merge(a, b Profile) (Profile, error) {
	if err := validProfile(a); err != nil {
		return Profile{}, err
	}
	if err := validProfile(b); err != nil {
		return Profile{}, err
	}
	if !a.Enabled() {
		return b, nil
	}
	if !b.Enabled() {
		return a, nil
	}
	return Profile{Version: 1, Tools: a.Tools | b.Tools}, nil
}

func toolBit(name string) uint32 {
	switch name {
	case "spawn_agent":
		return ToolSpawnAgent
	case "send_message":
		return ToolSendMessage
	case "followup_task":
		return ToolFollowupTask
	case "wait_agent":
		return ToolWaitAgent
	case "list_agents":
		return ToolListAgents
	case "interrupt_agent":
		return ToolInterruptAgent
	default:
		return 0
	}
}
func object(v any) map[string]any { m, _ := v.(map[string]any); return m }
func text(v any) string           { s, _ := v.(string); return s }
func copyMap(m map[string]any) map[string]any {
	n := make(map[string]any, len(m))
	for k, v := range m {
		n[k] = v
	}
	return n
}

// identity only recognizes explicit namespace or dotted namespace identities.
// A conflicting split/dotted identity is never treated as collaboration.
func identity(m map[string]any) (string, string) {
	name, ns := text(m["name"]), text(m["namespace"])
	if prefix, leaf, ok := strings.Cut(name, "."); ok {
		if ns != "" && ns != prefix {
			return "", ""
		}
		return prefix, leaf
	}
	return ns, name
}
func reservedName(m map[string]any) bool {
	name := text(m["name"])
	return name == Alias || strings.HasPrefix(name, Alias+".") || text(m["namespace"]) == Alias
}
func schemaMessage(m map[string]any) map[string]any {
	return object(object(object(m["parameters"])["properties"])["message"])
}
func nativeMessageSpec(m map[string]any) bool {
	return text(m["type"]) == "function" && toolBit(text(m["name"]))&messageTools != 0 && schemaMessage(m)["encrypted"] == true
}

// toolsAt visits only actual tool declaration containers, never schemas or
// arbitrary JSON. Namespace nesting is supported regardless of its outer name.
func toolsAt(body map[string]any, visit func([]any, string) error) error {
	if tools, ok := body["tools"].([]any); ok {
		if err := visit(tools, "tools"); err != nil {
			return err
		}
	}
	input, _ := body["input"].([]any)
	for i, value := range input {
		m := object(value)
		if text(m["type"]) == "additional_tools" {
			if tools, ok := m["tools"].([]any); ok {
				if err := visit(tools, fmt.Sprintf("input[%d].tools", i)); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// Detect identifies V2 declarations, native collaboration call history and
// agent_message history, without consulting User-Agent. Reserved user tools fail.
func Detect(body map[string]any) (Profile, error) {
	var p Profile
	if err := toolsAt(body, func(tools []any, path string) error { return detectTools(tools, path, &p) }); err != nil {
		return Profile{}, err
	}
	input, _ := body["input"].([]any)
	for _, value := range input {
		m := object(value)
		switch text(m["type"]) {
		case "agent_message":
			p.Version = 1
		case "function_call":
			ns, name := identity(m)
			if bit := toolBit(name); ns == Namespace && bit != 0 {
				p.Version = 1
				p.Tools |= bit
			}
		}
	}
	return p, nil
}

// mapArray copies a protocol array only when at least one child changes.
func mapArray(values []any, path string, fn func(map[string]any, string) (map[string]any, bool, error)) ([]any, bool, error) {
	var result []any
	for i, value := range values {
		m, ok := value.(map[string]any)
		if !ok {
			continue
		}
		n, changed, err := fn(m, fmt.Sprintf("%s[%d]", path, i))
		if err != nil {
			return nil, false, err
		}
		if changed {
			if result == nil {
				result = append([]any(nil), values...)
			}
			result[i] = n
		}
	}
	if result == nil {
		return values, false, nil
	}
	return result, true, nil
}

func stripSchema(m map[string]any) (map[string]any, bool) {
	message := schemaMessage(m)
	if _, exists := message["encrypted"]; !exists {
		return m, false
	}
	n := copyMap(m)
	params := copyMap(object(m["parameters"]))
	properties := copyMap(object(params["properties"]))
	message = copyMap(message)
	delete(message, "encrypted")
	properties["message"] = message
	params["properties"] = properties
	n["parameters"] = params
	return n, true
}

func forwardTools(tools []any, path string, p Profile) ([]any, bool, error) {
	return mapArray(tools, path, func(m map[string]any, at string) (map[string]any, bool, error) { return forwardTool(m, at, p) })
}

// emptyMarker accepts absent/null (serde Option::None) and empty arrays only.
func emptyMarker(m map[string]any, path string) error {
	marker := m["encrypted_function_args"]
	if marker == nil {
		return nil
	}
	switch values := marker.(type) {
	case []any:
		if len(values) == 0 {
			return nil
		}
	case []string:
		if len(values) == 0 {
			return nil
		}
	}
	return protocolError("codex_collaboration_encrypted", path+".encrypted_function_args", "encrypted or malformed arguments marker; regenerate a plaintext task from the parent agent")
}

// recognizableCiphertext checks a complete Fernet envelope, not merely a token
// prefix, so ordinary text mentioning gAAAAA is not reclassified as ciphertext.
func recognizableCiphertext(value string) bool {
	if !strings.HasPrefix(value, "gAAAA") {
		return false
	}
	decoded, err := base64.URLEncoding.DecodeString(value)
	if err != nil {
		decoded, err = base64.RawURLEncoding.DecodeString(value)
	}
	return err == nil && len(decoded) >= 73 && decoded[0] == 0x80 && (len(decoded)-57)%16 == 0
}

func plaintextArguments(m map[string]any, path string) error {
	if m["encrypted_content"] != nil {
		return protocolError("codex_collaboration_encrypted", path+".encrypted_content", "encrypted tool call; regenerate a plaintext task from the parent agent")
	}
	arguments, ok := m["arguments"].(string)
	if !ok {
		return protocolError("codex_collaboration_arguments", path+".arguments", "expected complete plaintext JSON arguments")
	}
	var params map[string]json.RawMessage
	if json.Unmarshal([]byte(arguments), &params) != nil || params == nil {
		return protocolError("codex_collaboration_arguments", path+".arguments", "expected complete plaintext JSON arguments")
	}
	var message string
	value, exists := params["message"]
	if !exists || json.Unmarshal(value, &message) != nil || string(value) == "null" {
		return protocolError("codex_collaboration_arguments", path+".arguments.message", "expected a plaintext message string")
	}
	if recognizableCiphertext(message) {
		return protocolError("codex_collaboration_encrypted", path+".arguments.message", "encrypted message; regenerate a plaintext task from the parent agent")
	}
	return nil
}

func renameIdentity(m map[string]any, namespace string) map[string]any {
	n := copyMap(m)
	_, name := identity(m)
	// Use the native split form for restoration. Forward preserves the client's
	// split/dotted representation; Canonical reconciles both for comparison.
	if namespace == Namespace || text(m["namespace"]) != "" {
		n["name"] = name
		n["namespace"] = namespace
	} else {
		n["name"] = namespace + "." + name
	}
	return n
}

func forwardCall(m map[string]any, path string, p Profile) (map[string]any, bool, error) {
	ns, name := identity(m)
	bit := toolBit(name)
	if (ns != Namespace && ns != Alias) || bit&p.Tools == 0 {
		return m, false, nil
	}
	if bit&messageTools != 0 {
		if err := emptyMarker(m, path); err != nil {
			return nil, false, err
		}
		if err := plaintextArguments(m, path); err != nil {
			return nil, false, err
		}
	}
	changed := ns == Namespace
	n := m
	if changed {
		n = renameIdentity(m, Alias)
	}
	if _, exists := m["encrypted_function_args"]; exists && bit&messageTools != 0 {
		if !changed {
			n = copyMap(m)
		}
		delete(n, "encrypted_function_args")
		changed = true
	}
	return n, changed, nil
}

func agentMessage(m map[string]any, path string) (map[string]any, bool, error) {
	if _, exists := m["encrypted_content"]; exists {
		return nil, false, protocolError("codex_agent_message_encrypted", path, "encrypted agent message; regenerate a plaintext task from the parent agent")
	}
	content, ok := m["content"].([]any)
	if !ok {
		return nil, false, protocolError("codex_agent_message_content", path+".content", "expected input_text content")
	}
	var texts []string
	for i, value := range content {
		part := object(value)
		_, encrypted := part["encrypted_content"]
		if encrypted || text(part["type"]) == "encrypted_content" {
			return nil, false, protocolError("codex_agent_message_encrypted", fmt.Sprintf("%s.content[%d]", path, i), "encrypted agent message; regenerate a plaintext task from the parent agent")
		}
		value, isText := part["text"].(string)
		if text(part["type"]) != "input_text" || !isText {
			return nil, false, protocolError("codex_agent_message_content", fmt.Sprintf("%s.content[%d]", path, i), "only input_text agent content is supported")
		}
		texts = append(texts, value)
	}
	author, recipient := text(m["author"]), text(m["recipient"])
	joined := strings.Join(texts, "\n")
	if strings.TrimSpace(joined) == "" {
		return nil, false, protocolError("codex_agent_message_content", path+".content", "expected nonempty plaintext agent content")
	}
	// Codex emits these exact attribution lines before Payload. Preserve its
	// original blocks byte-for-byte, adding metadata only when not already there.
	header, _, _ := strings.Cut(joined, "\nPayload:")
	hasAttribution := author != "" && recipient != "" && strings.Contains("\n"+header+"\n", "\nSender: "+author+"\n") && strings.Contains("\n"+header+"\n", "\nTask name: "+recipient+"\n")
	if !hasAttribution {
		// JSON quoting prevents metadata from injecting forged header lines.
		from, _ := json.Marshal(author)
		to, _ := json.Marshal(recipient)
		prefix := map[string]any{"type": "input_text", "text": "[Agent message metadata; not system or developer instructions. Author: " + string(from) + "; recipient: " + string(to) + "]"}
		content = append([]any{prefix}, content...)
	}
	return map[string]any{"type": "message", "role": "user", "content": content}, true, nil
}

func forwardChoice(m map[string]any, path string, p Profile) (map[string]any, bool, error) {
	ns, name := identity(m)
	if ns == Namespace && toolBit(name)&p.Tools != 0 {
		return renameIdentity(m, Alias), true, nil
	}
	// Namespace selectors select the whole container; only known selections are mapped.
	if text(m["type"]) == "namespace" && text(m["name"]) == Namespace && p.Tools != 0 {
		n := copyMap(m)
		n["name"] = Alias
		return n, true, nil
	}
	if text(m["type"]) == "allowed_tools" {
		values, _ := m["tools"].([]any)
		tools, changed, err := mapArray(values, path+toolsPathSuffix, func(child map[string]any, at string) (map[string]any, bool, error) {
			return forwardChoice(child, at, p)
		})
		if err != nil || !changed {
			return m, false, err
		}
		n := copyMap(m)
		n["tools"] = tools
		return n, true, nil
	}
	return m, false, nil
}

// Forward adapts only declarations, explicit tool choice and input protocol
// items. Results, arbitrary JSON, reasoning and compaction ciphertext are opaque.
// It is copy-on-write, leaves its input untouched, and is idempotent.
func Forward(body map[string]any, p Profile) (map[string]any, bool, error) {
	if err := validProfile(p); err != nil {
		return nil, false, err
	}
	if !p.Enabled() {
		return body, false, nil
	}
	n := body
	changed := false
	transforms := []struct {
		key   string
		apply func() (any, bool, error)
	}{
		{"tools", func() (any, bool, error) {
			tools, ok := body["tools"].([]any)
			if !ok {
				return nil, false, nil
			}
			return forwardTools(tools, "tools", p)
		}},
		{"input", func() (any, bool, error) {
			input, ok := body["input"].([]any)
			if !ok {
				return nil, false, nil
			}
			return mapArray(input, "input", func(m map[string]any, at string) (map[string]any, bool, error) { return forwardInput(m, at, p) })
		}},
		{"tool_choice", func() (any, bool, error) {
			choice := object(body["tool_choice"])
			if choice == nil {
				return nil, false, nil
			}
			return forwardChoice(choice, "tool_choice", p)
		}},
	}
	for _, transform := range transforms {
		value, fieldChanged, err := transform.apply()
		if err != nil {
			return nil, false, err
		}
		if !fieldChanged {
			continue
		}
		if !changed {
			n = copyMap(body)
			changed = true
		}
		n[transform.key] = value
	}
	return n, changed, nil
}

func restoreCall(m map[string]any, path string, p Profile, terminal bool) (map[string]any, bool, error) {
	if text(m["type"]) != "function_call" {
		return m, false, nil
	}
	ns, name := identity(m)
	bit := toolBit(name)
	if (ns != Alias && ns != Namespace) || bit&p.Tools == 0 {
		return m, false, nil
	}
	if bit&messageTools != 0 {
		if err := validateRestoredMessage(m, path, terminal); err != nil {
			return nil, false, err
		}
	}
	changed := ns == Alias || text(m["namespace"]) != Namespace || text(m["name"]) != name
	n := m
	if changed {
		n = renameIdentity(m, Namespace)
	}
	if bit&messageTools != 0 {
		n, changed = restorePlainMarker(m, n, changed)
	}

	return n, changed, nil
}

// restoreTools reverses only registered alias declarations in protocol tool
// containers. Schema restoration is necessary for compact histories to remain
// independently detectable after replay, without trusting arbitrary alias tools.
func restoreTools(tools []any, path string, p Profile) ([]any, bool, error) {
	aliases, natives := 0, 0
	for _, value := range tools {
		m := object(value)
		if text(m["type"]) == "namespace" {
			if text(m["name"]) == Alias {
				aliases++
			}
			if text(m["name"]) == Namespace {
				natives++
			}
		}
	}
	if aliases > 0 && aliases+natives > 1 {
		return nil, false, protocolError("codex_collaboration_alias_conflict", path, "conflicting collaboration namespace declarations")
	}
	return mapArray(tools, path, func(m map[string]any, at string) (map[string]any, bool, error) { return restoreTool(m, at, p) })
}

func restoreItem(m map[string]any, path string, p Profile, terminal bool) (map[string]any, bool, error) {
	if text(m["type"]) != "additional_tools" {
		return restoreCall(m, path, p, terminal)
	}
	tools, _ := m["tools"].([]any)
	values, changed, err := restoreTools(tools, path+toolsPathSuffix, p)
	if err != nil || !changed {
		return m, false, err
	}
	n := copyMap(m)
	n["tools"] = values
	return n, true, nil
}

// RestoreBody restores terminal Responses/compact output, including response
// wrappers and additional_tools declarations. Validation occurs before any
// modified value is returned.
func RestoreBody(body map[string]any, p Profile) (map[string]any, bool, error) {
	if err := validProfile(p); err != nil {
		return nil, false, err
	}
	if !p.Enabled() {
		return body, false, nil
	}
	return restoreBody(body, p, "body")
}
func restoreBody(body map[string]any, p Profile, path string) (map[string]any, bool, error) {
	n := body
	changed := false
	if output, ok := body["output"].([]any); ok {
		values, c, err := mapArray(output, path+".output", func(m map[string]any, at string) (map[string]any, bool, error) { return restoreItem(m, at, p, true) })
		if err != nil {
			return nil, false, err
		}
		if c {
			n = copyMap(body)
			changed = true
			n["output"] = values
		}
	}
	if response := object(body["response"]); response != nil {
		value, c, err := restoreBody(response, p, path+".response")
		if err != nil {
			return nil, false, err
		}
		if c {
			if !changed {
				n = copyMap(body)
				changed = true
			}
			n["response"] = value
		}
	}
	return n, changed, nil
}

// RestoreEvent preserves argument deltas and all non-protocol event data. Added
// items may have incomplete arguments; done items and output snapshots may not.
func RestoreEvent(event map[string]any, p Profile) (map[string]any, bool, error) {
	if err := validProfile(p); err != nil {
		return nil, false, err
	}
	if !p.Enabled() {
		return event, false, nil
	}
	typ := text(event["type"])
	n, changed, err := restoreEventItem(event, p, typ)
	if err != nil {
		return nil, false, err
	}

	if typ == "response.function_call_arguments.delta" || typ == "response.function_call_arguments.done" {
		ns, name := identity(event)
		if ns == Alias && toolBit(name)&p.Tools != 0 {
			n = renameIdentity(event, Namespace)
			changed = true
		}
	}
	value, c, err := restoreBody(n, p, "event")
	if err != nil {
		return nil, false, err
	}
	return value, changed || c, nil
}

// Canonical is a comparison-only projection. It never changes arguments and
// never normalizes unregistered tools, nonempty markers, or arbitrary JSON.
func Canonical(item any, p Profile) any {
	if !p.Enabled() {
		return item
	}
	if items, ok := item.([]any); ok {
		return canonicalItems(items, p)
	}
	m := object(item)
	if text(m["type"]) != "function_call" {
		return item
	}
	ns, name := identity(m)
	bit := toolBit(name)
	if (ns != Namespace && ns != Alias) || bit&p.Tools == 0 {
		return item
	}
	n := copyMap(m)
	n["namespace"] = Alias
	n["name"] = name
	if bit&messageTools != 0 && emptyMarker(m, "item") == nil {
		delete(n, "encrypted_function_args")
	}
	return n
}

func forwardTool(m map[string]any, at string, p Profile) (map[string]any, bool, error) {
	if text(m["type"]) != "namespace" {
		return m, false, nil
	}
	children, _ := m["tools"].([]any)
	if text(m["name"]) == Namespace {
		matched := false
		for _, child := range children {
			cm := object(child)
			bit := toolBit(text(cm["name"]))
			if text(cm["type"]) == "function" && bit&p.Tools != 0 {
				matched = true
			}
		}
		if matched {
			return forwardNamespace(m, children, at, p)
		}
	}
	cleaned, changed, err := forwardTools(children, at+toolsPathSuffix, p)
	if err != nil || !changed {
		return m, false, err
	}
	n := copyMap(m)
	n["tools"] = cleaned
	return n, true, nil
}

func forwardInput(m map[string]any, at string, p Profile) (map[string]any, bool, error) {
	switch text(m["type"]) {
	case "agent_message":
		return agentMessage(m, at)
	case "function_call":
		return forwardCall(m, at, p)
	case "additional_tools":
		tools, _ := m["tools"].([]any)
		values, c, err := forwardTools(tools, at+toolsPathSuffix, p)
		if err != nil || !c {
			return m, false, err
		}
		n := copyMap(m)
		n["tools"] = values
		return n, true, nil
	}
	return m, false, nil
}

func restoreTool(m map[string]any, at string, p Profile) (map[string]any, bool, error) {
	if text(m["type"]) != "namespace" {
		if reservedName(m) {
			return nil, false, protocolError("codex_collaboration_tool", at, "alias declaration is not a registered namespace")
		}
		return m, false, nil
	}
	if text(m["name"]) == Alias {
		return restoreAliasNamespace(m, at, p)
	}
	if reservedName(m) {
		return nil, false, protocolError("codex_collaboration_tool", at, "invalid alias namespace declaration")
	}
	children, _ := m["tools"].([]any)
	values, changed, err := restoreTools(children, at+toolsPathSuffix, p)
	if err != nil || !changed {
		return m, false, err
	}
	n := copyMap(m)
	n["tools"] = values
	return n, true, nil
}

func detectTools(tools []any, path string, p *Profile) error {
	for i, value := range tools {
		m := object(value)
		at := fmt.Sprintf("%s[%d]", path, i)
		if reservedName(m) {
			return protocolError("codex_collaboration_alias_conflict", at, "reserved collaboration alias is already defined")
		}
		if text(m["type"]) != "namespace" {
			continue
		}
		children, _ := m["tools"].([]any)
		if text(m["name"]) == Namespace {
			detectNamespaceTools(children, p)
		}
		if err := detectTools(children, at+toolsPathSuffix, p); err != nil {
			return err
		}
	}
	return nil
}
func detectNamespaceTools(children []any, p *Profile) {
	var bits uint32
	matched := false
	for _, child := range children {
		cm := object(child)
		if nativeMessageSpec(cm) {
			matched = true
		}
		if text(cm["type"]) == "function" {
			bits |= toolBit(text(cm["name"]))
		}
	}
	if matched {
		p.Version = 1
		p.Tools |= bits
	}
}

func restoreAliasNamespace(m map[string]any, at string, p Profile) (map[string]any, bool, error) {
	children, ok := m["tools"].([]any)
	if !ok || len(children) == 0 {
		return nil, false, protocolError("codex_collaboration_tool", at, "alias namespace has no registered declarations")
	}
	seen := uint32(0)
	for _, value := range children {
		child := object(value)
		bit := toolBit(text(child["name"]))
		if text(child["type"]) != "function" || bit == 0 || bit&p.Tools == 0 || bit&seen != 0 {
			return nil, false, protocolError("codex_collaboration_tool", at+toolsPathSuffix, "unregistered or duplicate alias declaration")
		}
		seen |= bit
		if bit&messageTools != 0 && schemaMessage(child) == nil {
			return nil, false, protocolError("codex_collaboration_tool", at+toolsPathSuffix, "message declaration is missing its schema")
		}
	}
	values, _, err := mapArray(children, at+toolsPathSuffix, func(child map[string]any, _ string) (map[string]any, bool, error) {
		if toolBit(text(child["name"]))&messageTools == 0 {
			return child, false, nil
		}
		if schemaMessage(child)["encrypted"] == true {
			return child, false, nil
		}
		n := copyMap(child)
		params := copyMap(object(child["parameters"]))
		properties := copyMap(object(params["properties"]))
		message := copyMap(schemaMessage(child))
		message["encrypted"] = true
		properties["message"] = message
		params["properties"] = properties
		n["parameters"] = params
		return n, true, nil
	})
	if err != nil {
		return nil, false, err
	}
	n := copyMap(m)
	n["name"] = Namespace
	n["tools"] = values
	return n, true, nil
}

func forwardNamespace(m map[string]any, children []any, at string, p Profile) (map[string]any, bool, error) {
	// Renaming a namespace would also rename unknown members. Do not
	// silently advertise them as official tools on the return path.
	for _, child := range children {
		cm := object(child)
		if text(cm["type"]) != "function" || toolBit(text(cm["name"]))&p.Tools == 0 {
			return nil, false, protocolError("codex_collaboration_tool", at, "unregistered collaboration member")
		}
	}
	n := copyMap(m)
	n["name"] = Alias
	cleaned, _, err := mapArray(children, at+toolsPathSuffix, func(cm map[string]any, _ string) (map[string]any, bool, error) {
		if toolBit(text(cm["name"]))&messageTools != 0 {
			next, changed := stripSchema(cm)
			return next, changed, nil
		}
		return cm, false, nil
	})
	if err != nil {
		return nil, false, err
	}
	n["tools"] = cleaned
	return n, true, nil
}

func validateRestoredMessage(m map[string]any, path string, terminal bool) error {
	if err := emptyMarker(m, path); err != nil {
		return err
	}
	if m["encrypted_content"] != nil {
		return protocolError("codex_collaboration_encrypted", path+".encrypted_content", "encrypted tool call cannot be restored as plaintext")
	}
	if terminal {
		if err := plaintextArguments(m, path); err != nil {
			return err
		}
	}
	return nil
}

func canonicalItems(items []any, p Profile) any {
	var result []any
	for i, value := range items {
		next := Canonical(value, p)
		if m := object(value); m != nil && text(m["type"]) == "function_call" {
			if result == nil {
				result = append([]any(nil), items...)
			}
			result[i] = next
		}
	}
	if result != nil {
		return result
	}
	return items
}

func restorePlainMarker(m, n map[string]any, changed bool) (map[string]any, bool) {
	plain := false
	switch marker := m["encrypted_function_args"].(type) {
	case []any:
		plain = marker != nil && len(marker) == 0
	case []string:
		plain = marker != nil && len(marker) == 0
	}
	if !plain || changed {
		if !changed {
			n = copyMap(m)
			changed = true
		}
		n["encrypted_function_args"] = []any{}
	}

	return n, changed
}

func restoreEventItem(event map[string]any, p Profile, typ string) (map[string]any, bool, error) {
	n := event
	changed := false
	if typ == "response.output_item.added" || typ == "response.output_item.done" {
		if item := object(event["item"]); item != nil {
			value, c, err := restoreItem(item, "event.item", p, typ == "response.output_item.done")
			if err != nil {
				return nil, false, err
			}
			if c {
				n = copyMap(event)
				changed = true
				n["item"] = value
			}
		}
	}
	return n, changed, nil
}
