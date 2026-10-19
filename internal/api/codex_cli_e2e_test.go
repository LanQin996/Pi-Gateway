package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"
)

const (
	codexCLIAlias        = "pi_codex_collaboration_v1"
	codexCLITask         = "CLI_TASK_a91e: 原文任务第一行\n第二行：保留 Unicode 与换行。"
	codexCLIMail         = "CLI_MAIL_b82f: 邮箱独有消息，不得改写。"
	codexCLIFollowup     = "CLI_FOLLOWUP_c73d: 第二次启动同一子代理。"
	codexCLINestedTask   = "CLI_NESTED_d64c: 只向直接父代理报告。"
	codexCLINestedAnswer = "CLI_NESTED_RESULT_e55b: 嵌套完成。"
	codexCLIAnswer1      = "CLI_CHILD_FINAL_1_f46a: 第一轮完成，已收到原文任务和邮箱。"
	codexCLIAnswer2      = "CLI_CHILD_FINAL_2_a37f: 第二轮独有结果。"
	codexCLIFinal        = "CLI_GATEWAY_SUCCESS: " + codexCLIAnswer1 + " | " + codexCLIAnswer2
)

// TestCodexCLIMultiAgentV2 executes the real client, not a hand-written client
// lookalike. The only model server is the strict local script below. In
// particular, it never produces shell, MCP, web-search, or filesystem calls.
func TestCodexCLIMultiAgentV2(t *testing.T) {
	if os.Getenv("PI_GATEWAY_CODEX_E2E") != "1" {
		t.Skip("set PI_GATEWAY_CODEX_E2E=1 to run the isolated real Codex CLI")
	}
	binary := codexCLIBinary(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	root := t.TempDir()
	home, workspace := filepath.Join(root, "codex-home"), filepath.Join(root, "workspace")
	for _, dir := range []string{home, workspace, filepath.Join(root, "tmp"), filepath.Join(root, "appdata"), filepath.Join(root, "localappdata")} {
		if err := os.MkdirAll(dir, 0700); err != nil {
			t.Fatal(err)
		}
	}
	// No user environment (including auth, proxies, hooks, or provider URLs) is
	// inherited. HOME/USERPROFILE are private as well as CODEX_HOME.
	env := codexCLIEnvironment(root, home)
	versionCmd := exec.CommandContext(ctx, binary, "--version")
	versionCmd.Env, versionCmd.Dir = env, workspace
	version, err := versionCmd.CombinedOutput()
	if err != nil {
		t.Fatalf("Codex version: %v: %s", err, version)
	}
	t.Logf("real CLI binary=%s version=%s", binary, bytes.TrimSpace(version))

	script := &codexCLIScript{t: t, cancel: cancel, stages: map[string]int{}, calls: map[string]map[string]any{}, mailDelivered: make(chan struct{}), nestedWaiting: make(chan struct{})}
	h := newHarness(t, script, "sse")
	t.Cleanup(h.dataPlane.Close)
	catalog := codexCLIWriteCatalog(t, root)
	args := []string{"exec", "--json", "--ephemeral", "--ignore-user-config", "--ignore-rules", "--enable", "multi_agent", "--enable", "multi_agent_v2", "--sandbox", "read-only", "--skip-git-repo-check", "--color", "never", "--cd", workspace, "--model", "pi-codex-e2e"}
	for _, setting := range []string{
		`model_provider="gateway_e2e"`,
		`model_catalog_json=` + codexCLITOML(catalog),
		`agents.max_depth=3`,
		`multi_agent_v2.max_concurrent_threads_per_session=4`,
		`model_providers.gateway_e2e.name="isolated gateway e2e"`,
		`model_providers.gateway_e2e.base_url=` + codexCLITOML(h.server.URL+"/v1"),
		`model_providers.gateway_e2e.env_key="PI_CODEX_E2E_KEY"`,
		`model_providers.gateway_e2e.wire_api="responses"`,
		`model_providers.gateway_e2e.requires_openai_auth=false`,
		`model_providers.gateway_e2e.supports_websockets=false`,
		`model_providers.gateway_e2e.request_max_retries=0`,
		`model_providers.gateway_e2e.stream_max_retries=0`,
		`model_providers.gateway_e2e.stream_idle_timeout_ms=15000`,
		`approval_policy="never"`, `mcp_servers={}`, `notify=[]`,
		`analytics.enabled=false`, `feedback.enabled=false`,
		`otel.exporter="none"`, `otel.metrics_exporter="none"`,
		`features.apps=false`, `features.hooks=false`, `features.shell_tool=false`,
		`features.shell_snapshot=false`,
		`features.js_repl=false`, `features.exec_policy=false`,
		`web_search="disabled"`, `check_for_update_on_startup=false`,
		`multi_agent_v2.message_board_in_memory=true`,
		`multi_agent_v2.min_wait_timeout_ms=1`,
		`multi_agent_v2.default_wait_timeout_ms=15000`,
		`multi_agent_v2.max_wait_timeout_ms=15000`,
	} {
		args = append(args, "-c", setting)
	}
	args = append(args, "CLI_ROOT_748a: Run the scripted local collaboration protocol; do not execute shell commands.")
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Dir, cmd.Env, cmd.WaitDelay = workspace, env, 3*time.Second
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err = codexCLIRun(cmd)
	if err != nil || ctx.Err() != nil {
		t.Errorf("real Codex execution: %v (deadline: %v)\nstdout:\n%s\nstderr:\n%s", err, ctx.Err(), codexCLITail(stdout.String()), codexCLITail(stderr.String()))
	}
	script.assertFinished(t)
	if !strings.Contains(stdout.String(), codexCLIFinal) {
		t.Errorf("CLI JSONL lacks exact child-derived final answer %q; stdout:\n%s\nstderr:\n%s", codexCLIFinal, codexCLITail(stdout.String()), codexCLITail(stderr.String()))
	}
	// A dedicated empty workspace plus read-only sandbox is intentional: neither
	// host configuration nor a repository instruction can add executable tools.
	entries, readErr := os.ReadDir(workspace)
	if readErr != nil || len(entries) != 0 {
		t.Errorf("read-only workspace changed: entries=%v error=%v", entries, readErr)
	}
}

// Pin model-owned tool/backend selectors too: a catalog leaf model can disable
// collaboration in freshly spawned children despite the root's feature flags.
func codexCLIWriteCatalog(t *testing.T, root string) string {
	t.Helper()
	catalog := map[string]any{"models": []any{map[string]any{
		"slug": "pi-codex-e2e", "display_name": "Local scripted CLI model",
		"description":                "Test-only local model; never contact a real provider",
		"supported_reasoning_levels": []any{}, "shell_type": "disabled",
		"visibility": "list", "supported_in_api": true, "priority": 0,
		"support_verbosity": false, "truncation_policy": map[string]any{"mode": "tokens", "limit": 100000},
		"experimental_supported_tools": []any{}, "context_window": 200000,
		"multi_agent_version": "v2", "tool_mode": "direct",
		"base_instructions": "Execute only the scripted collaboration calls. Never run shell commands or edit files.",
	}}}
	encoded, err := json.Marshal(catalog)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "models.json")
	if err := os.WriteFile(path, encoded, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func codexCLITOML(s string) string { b, _ := json.Marshal(s); return string(b) }

func codexCLITail(s string) string {
	const limit = 24000
	if len(s) > limit {
		return "[earlier output omitted]\n" + s[len(s)-limit:]
	}
	return s
}

// npm's Windows codex.cmd cannot be passed directly to exec.Command. Resolve
// its adjacent package's native executable instead of launching cmd.exe or a
// shell (and therefore leave no wrapper process tree behind on cancellation).
func codexCLIBinary(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("codex")
	if err == nil && (runtime.GOOS != "windows" || strings.EqualFold(filepath.Ext(path), ".exe")) {
		return path
	}
	var dirs []string
	if path != "" {
		dirs = append(dirs, filepath.Dir(path))
	}
	dirs = append(dirs, filepath.SplitList(os.Getenv("PATH"))...)
	for _, dir := range dirs {
		matches, _ := filepath.Glob(filepath.Join(dir, "node_modules", "@openai", "codex", "node_modules", "@openai", "codex-win32-*", "vendor", "*", "bin", "codex.exe"))
		if len(matches) > 0 {
			return matches[0]
		}
	}
	t.Fatalf("PI_GATEWAY_CODEX_E2E=1 but native Codex CLI is unavailable (LookPath: %v)", err)
	return ""
}

func codexCLIEnvironment(root, home string) []string {
	var env []string
	for _, name := range []string{"PATH", "PATHEXT", "SYSTEMROOT", "WINDIR", "COMSPEC", "PROCESSOR_ARCHITECTURE", "NUMBER_OF_PROCESSORS"} {
		if value, ok := os.LookupEnv(name); ok {
			env = append(env, name+"="+value)
		}
	}
	return append(env,
		"CODEX_HOME="+home, "HOME="+root, "USERPROFILE="+root,
		"APPDATA="+filepath.Join(root, "appdata"), "LOCALAPPDATA="+filepath.Join(root, "localappdata"),
		"TEMP="+filepath.Join(root, "tmp"), "TMP="+filepath.Join(root, "tmp"),
		"PI_CODEX_E2E_KEY=sk-pi-test", "RUST_LOG=error", "NO_COLOR=1",
		// Defense in depth: even accidental remote HTTP clients cannot reach an
		// authenticated provider. Only loopback endpoints bypass this dead proxy.
		"HTTP_PROXY=http://127.0.0.1:9", "HTTPS_PROXY=http://127.0.0.1:9", "ALL_PROXY=http://127.0.0.1:9",
		"http_proxy=http://127.0.0.1:9", "https_proxy=http://127.0.0.1:9", "all_proxy=http://127.0.0.1:9",
		"NO_PROXY=127.0.0.1,localhost,::1", "no_proxy=127.0.0.1,localhost,::1")
}

type codexCLIScript struct {
	t                 *testing.T
	cancel            context.CancelFunc
	mu                sync.Mutex
	stages            map[string]int
	calls             map[string]map[string]any
	violations        []string
	aliasRequests     int
	convertedMessages int
	outputs           map[string]bool
	mailDelivered     chan struct{}
	nestedWaiting     chan struct{}
}

func (s *codexCLIScript) reject(w http.ResponseWriter, format string, args ...any) {
	message := fmt.Sprintf(format, args...)
	s.mu.Lock()
	s.violations = append(s.violations, message)
	s.mu.Unlock()
	s.t.Logf("strict fake upstream rejection: %s", message)
	http.Error(w, message, http.StatusBadRequest)
	s.cancel()
}

func (s *codexCLIScript) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") {
		s.reject(w, "unexpected upstream endpoint %s %s", r.Method, r.URL.Path)
		return
	}
	var request map[string]any
	if err := json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&request); err != nil {
		s.reject(w, "decode request: %v", err)
		return
	}
	messages, err := s.validate(request)
	if err != nil {
		s.reject(w, "%v", err)
		return
	}
	role := "root"
	for _, text := range messages {
		if strings.Contains(text, "Message Type: NEW_TASK\nTask name: /root/worker/nested\n") {
			role = "nested"
			break
		}
		if strings.Contains(text, "Message Type: NEW_TASK\nTask name: /root/worker\n") {
			role = "worker"
		}
	}
	s.mu.Lock()
	stage := s.stages[role]
	s.stages[role]++
	s.mu.Unlock()
	s.t.Logf("CLI local upstream role=%s stage=%d messages=%d", role, stage, len(messages))
	joined := strings.Join(messages, "\n")
	need := func(text string) bool {
		if !strings.Contains(joined, text) {
			s.reject(w, "%s stage %d lacks exact converted message %q", role, stage, text)
			return false
		}
		return true
	}
	output := func(call string) bool {
		if err := s.requireOutput(request, call); err != nil {
			s.reject(w, "%s stage %d: %v", role, stage, err)
			return false
		}
		return true
	}
	call := func(id, name string, args map[string]any) { s.stream(w, role, stage, s.call(id, name, args)) }
	wait := func(id string) { call(id, "wait_agent", map[string]any{"timeout_ms": 15000}) }
	await := func(latch <-chan struct{}) bool {
		select {
		case <-latch:
			return true
		case <-r.Context().Done():
			return false
		}
	}
	switch role {
	case "root":
		// The grandchild's automatic FINAL_ANSWER must go only to its immediate
		// parent, not leak as an agent notification into the root mailbox.
		if strings.Contains(joined, "Sender: /root/worker/nested\n") {
			s.reject(w, "grandchild notification leaked into root mailbox")
			return
		}
		switch stage {
		case 0:
			call("cli_spawn_worker", "spawn_agent", map[string]any{"task_name": "worker", "message": codexCLITask, "fork_turns": "none"})
		case 1:
			if !output("cli_spawn_worker") {
				return
			}
			call("cli_send_worker", "send_message", map[string]any{"target": "/root/worker", "message": codexCLIMail})
		case 2:
			if !output("cli_send_worker") {
				return
			}
			close(s.mailDelivered)
			wait("cli_wait_first")
		case 3:
			if !output("cli_wait_first") || !need("Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/worker\nPayload:\n"+codexCLIAnswer1) {
				return
			}
			call("cli_followup_worker", "followup_task", map[string]any{"target": "/root/worker", "message": codexCLIFollowup})
		case 4:
			if !output("cli_followup_worker") {
				return
			}
			wait("cli_wait_second")
		case 5:
			if !output("cli_wait_second") || !need("Message Type: FINAL_ANSWER\nTask name: /root\nSender: /root/worker\nPayload:\n"+codexCLIAnswer2) {
				return
			}
			s.stream(w, role, stage, codexCLIMessage("msg_cli_root", codexCLIFinal))
		default:
			s.reject(w, "unexpected root stage %d", stage)
		}
	case "worker":
		if !need("Message Type: NEW_TASK\nTask name: /root/worker\nSender: /root\nPayload:\n" + codexCLITask) {
			return
		}
		switch stage {
		case 0:
			if !await(s.mailDelivered) {
				return
			}
			wait("cli_worker_mailbox")
		case 1:
			if !output("cli_worker_mailbox") || !need("Message Type: MESSAGE\nTask name: /root/worker\nSender: /root\nPayload:\n"+codexCLIMail) {
				return
			}
			call("cli_spawn_nested", "spawn_agent", map[string]any{"task_name": "nested", "message": codexCLINestedTask, "fork_turns": "none"})
		case 2:
			if !output("cli_spawn_nested") {
				return
			}
			close(s.nestedWaiting)
			wait("cli_wait_nested")
		case 3:
			if !output("cli_wait_nested") || !need("Message Type: FINAL_ANSWER\nTask name: /root/worker\nSender: /root/worker/nested\nPayload:\n"+codexCLINestedAnswer) {
				return
			}
			s.stream(w, role, stage, codexCLIMessage("msg_cli_worker_first", codexCLIAnswer1))
		case 4:
			if !need(codexCLIFollowup) {
				return
			}
			s.stream(w, role, stage, codexCLIMessage("msg_cli_worker_second", codexCLIAnswer2))
		default:
			s.reject(w, "unexpected worker stage %d", stage)
		}
	case "nested":
		if stage != 0 || !need("Message Type: NEW_TASK\nTask name: /root/worker/nested\nSender: /root/worker\nPayload:\n"+codexCLINestedTask) {
			s.reject(w, "invalid nested task or stage %d", stage)
			return
		}
		if !await(s.nestedWaiting) {
			return
		}
		s.stream(w, role, stage, codexCLIMessage("msg_cli_nested", codexCLINestedAnswer))
	}
}

func (s *codexCLIScript) validate(request map[string]any) ([]string, error) {
	var messages []string
	alias := false
	var tools func(any) error
	tools = func(value any) error {
		switch v := value.(type) {
		case []any:
			for _, child := range v {
				if err := tools(child); err != nil {
					return err
				}
			}
		case map[string]any:
			if v["type"] == "namespace" {
				if v["name"] == "collaboration" {
					return fmt.Errorf("native collaboration namespace reached upstream")
				}
				if v["name"] == codexCLIAlias {
					alias = true
				}
			}
			if p, ok := v["parameters"].(map[string]any); ok {
				if props, ok := p["properties"].(map[string]any); ok {
					if m, ok := props["message"].(map[string]any); ok {
						if _, exists := m["encrypted"]; exists {
							return fmt.Errorf("message.encrypted schema reached upstream")
						}
					}
				}
			}
			for _, key := range []string{"tools", "function"} {
				if child, ok := v[key]; ok {
					if err := tools(child); err != nil {
						return err
					}
				}
			}
		}
		return nil
	}
	if err := tools(request["tools"]); err != nil {
		return nil, err
	}
	input, ok := request["input"].([]any)
	if !ok {
		return nil, fmt.Errorf("input is not an array")
	}
	for _, raw := range input {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("non-object input")
		}
		if item["type"] == "agent_message" {
			return nil, fmt.Errorf("agent_message reached strict upstream")
		}
		if item["type"] == "function_call" {
			ns, _ := item["namespace"].(string)
			name, _ := item["name"].(string)
			if ns == "collaboration" || strings.HasPrefix(name, "collaboration.") {
				return nil, fmt.Errorf("native collaboration history reached upstream")
			}
			if _, exists := item["encrypted_function_args"]; exists {
				return nil, fmt.Errorf("encrypted_function_args marker reached alias upstream")
			}
		}
		if err := tools(item["additional_tools"]); err != nil {
			return nil, err
		}
		var text strings.Builder
		switch content := item["content"].(type) {
		case string:
			text.WriteString(content)
		case []any:
			for _, rawPart := range content {
				part, ok := rawPart.(map[string]any)
				if !ok {
					continue
				}
				if part["type"] == "encrypted_content" {
					return nil, fmt.Errorf("encrypted_content in message reached upstream")
				}
				if value, ok := part["text"].(string); ok {
					text.WriteString(value)
				}
			}
		}
		if strings.HasPrefix(text.String(), "Message Type:") {
			if item["type"] != "message" || item["role"] != "user" {
				return nil, fmt.Errorf("converted agent item is not message/user")
			}
			messages = append(messages, text.String())
		}
	}
	if !alias {
		summary := map[string]any{"tools": request["tools"], "messages": messages, "client_metadata": request["client_metadata"]}
		encoded, _ := json.Marshal(summary)
		return nil, fmt.Errorf("request does not advertise alias namespace %s: %s", codexCLIAlias, codexCLITail(string(encoded)))
	}
	s.mu.Lock()
	s.aliasRequests++
	s.convertedMessages += len(messages)
	s.mu.Unlock()
	return messages, nil
}

func (s *codexCLIScript) requireOutput(request map[string]any, id string) error {
	var call, output map[string]any
	for _, raw := range request["input"].([]any) {
		item, _ := raw.(map[string]any)
		if item["call_id"] == id {
			switch item["type"] {
			case "function_call":
				call = item
			case "function_call_output":
				output = item
			}
		}
	}
	if call == nil || output == nil {
		return fmt.Errorf("missing call/output association for %s", id)
	}
	s.mu.Lock()
	expected := s.calls[id]
	s.mu.Unlock()
	if expected == nil || call["arguments"] != expected["arguments"] {
		return fmt.Errorf("arguments changed for call_id=%s", id)
	}
	name, _ := expected["name"].(string)
	if !((call["namespace"] == codexCLIAlias && call["name"] == name) || call["name"] == codexCLIAlias+"."+name) {
		return fmt.Errorf("call identity changed for %s: %v/%v", id, call["namespace"], call["name"])
	}
	encoded, _ := json.Marshal(output["output"])
	for _, invalid := range []string{"Error", "error", "not found", "timed out", `timed_out\":true`} {
		if strings.Contains(string(encoded), invalid) {
			return fmt.Errorf("tool call %s failed: %s", id, encoded)
		}
	}
	s.mu.Lock()
	if s.outputs == nil {
		s.outputs = map[string]bool{}
	}
	s.outputs[id] = true
	s.mu.Unlock()
	return nil
}

func (s *codexCLIScript) call(id, name string, args map[string]any) map[string]any {
	encoded, _ := json.Marshal(args)
	item := map[string]any{"id": "fc_" + id, "type": "function_call", "call_id": id, "namespace": codexCLIAlias, "name": name, "arguments": string(encoded), "status": "completed"}
	// Deliberately omit encrypted_function_args from added, done, and completed.
	// The gateway MUST restore [] for all three message tools before execution.
	s.mu.Lock()
	s.calls[id] = item
	s.mu.Unlock()
	return item
}

func codexCLIMessage(id, text string) map[string]any {
	return map[string]any{"id": id, "type": "message", "role": "assistant", "status": "completed", "phase": "final_answer", "content": []any{map[string]any{"type": "output_text", "text": text, "annotations": []any{}}}}
}

func (s *codexCLIScript) stream(w http.ResponseWriter, role string, stage int, item map[string]any) {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Second))
	sequence := 0
	emit := func(event map[string]any) {
		event["sequence_number"] = sequence
		sequence++
		encoded, _ := json.Marshal(event)
		_, _ = fmt.Fprintf(w, "event: %s\ndata: %s\n\n", event["type"], encoded)
		w.(http.Flusher).Flush()
	}
	responseID := fmt.Sprintf("resp_cli_%s_%d", role, stage)
	emit(map[string]any{"type": "response.created", "response": map[string]any{"id": responseID, "object": "response", "created_at": time.Now().Unix(), "status": "in_progress", "output": []any{}}})
	added := map[string]any{}
	for k, v := range item {
		added[k] = v
	}
	added["status"] = "in_progress"
	if item["type"] == "function_call" {
		added["arguments"] = ""
	} else {
		added["content"] = []any{}
	}
	emit(map[string]any{"type": "response.output_item.added", "output_index": 0, "item": added})
	if item["type"] == "function_call" {
		args := item["arguments"].(string)
		// Split inside JSON, rather than only between JSON tokens. Byte values
		// must arrive unchanged even when argument fragments are incomplete.
		split := len(args) / 2
		for split > 1 && !utf8.RuneStart(args[split]) {
			split--
		}
		for _, delta := range []string{args[:1], args[1:split], args[split:]} {
			emit(map[string]any{"type": "response.function_call_arguments.delta", "item_id": item["id"], "output_index": 0, "delta": delta})
		}
		emit(map[string]any{"type": "response.function_call_arguments.done", "item_id": item["id"], "output_index": 0, "arguments": args})
	} else {
		part := item["content"].([]any)[0].(map[string]any)
		emit(map[string]any{"type": "response.content_part.added", "item_id": item["id"], "output_index": 0, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}}})
		emit(map[string]any{"type": "response.output_text.delta", "item_id": item["id"], "output_index": 0, "content_index": 0, "delta": part["text"]})
		emit(map[string]any{"type": "response.output_text.done", "item_id": item["id"], "output_index": 0, "content_index": 0, "text": part["text"]})
		emit(map[string]any{"type": "response.content_part.done", "item_id": item["id"], "output_index": 0, "content_index": 0, "part": part})
	}
	emit(map[string]any{"type": "response.output_item.done", "output_index": 0, "item": item})
	emit(map[string]any{"type": "response.completed", "response": map[string]any{"id": responseID, "object": "response", "created_at": time.Now().Unix(), "status": "completed", "output": []any{item}, "usage": map[string]any{"input_tokens": 100, "output_tokens": 20, "total_tokens": 120}}})
}

func (s *codexCLIScript) assertFinished(t *testing.T) {
	t.Helper()
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.violations) != 0 {
		t.Errorf("strict upstream violations: %v", s.violations)
	}
	for role, want := range map[string]int{"root": 6, "worker": 5, "nested": 1} {
		if s.stages[role] != want {
			t.Errorf("%s requests=%d want=%d; all stages=%v", role, s.stages[role], want, s.stages)
		}
	}
	for _, id := range []string{"cli_spawn_worker", "cli_send_worker", "cli_wait_first", "cli_followup_worker", "cli_wait_second", "cli_worker_mailbox", "cli_spawn_nested", "cli_wait_nested"} {
		if !s.outputs[id] {
			t.Errorf("missing correlated successful tool output %s", id)
		}
	}
	if s.aliasRequests != 12 || s.convertedMessages < 8 {
		t.Errorf("missing alias/converted message evidence: requests=%d messages=%d", s.aliasRequests, s.convertedMessages)
	}
	t.Logf("strict upstream: %d aliased requests, %d converted agent-message observations, %d correlated tool outputs", s.aliasRequests, s.convertedMessages, len(s.outputs))
}
