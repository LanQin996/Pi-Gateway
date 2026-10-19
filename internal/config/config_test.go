package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexMultiAgentCompatConfig(t *testing.T) {
	if !Default().Upstream.CodexMultiAgentCompat {
		t.Fatal("compatibility must be enabled by default")
	}
	for _, tc := range []struct {
		name, yaml, env string
		want            bool
	}{
		{"omitted", "upstream: {}\n", "", true},
		{"explicit_false", "upstream:\n  codex_multi_agent_compat: false\n", "", false},
		{"explicit_true", "upstream:\n  codex_multi_agent_compat: true\n", "", true},
		{"env_false", "upstream:\n  codex_multi_agent_compat: true\n", "false", false},
		{"env_true", "upstream:\n  codex_multi_agent_compat: false\n", "true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("PI_GATEWAY_UPSTREAM_CODEX_MULTI_AGENT_COMPAT", tc.env)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Upstream.CodexMultiAgentCompat != tc.want {
				t.Fatalf("compat = %v, want %v", cfg.Upstream.CodexMultiAgentCompat, tc.want)
			}
		})
	}
	t.Run("invalid_env_redacted", func(t *testing.T) {
		t.Setenv("PI_GATEWAY_UPSTREAM_CODEX_MULTI_AGENT_COMPAT", "private-invalid-value")
		_, err := Load("")
		if err == nil || !strings.Contains(err.Error(), "PI_GATEWAY_UPSTREAM_CODEX_MULTI_AGENT_COMPAT") || strings.Contains(err.Error(), "private-invalid-value") {
			t.Fatalf("invalid boolean was accepted or leaked: %v", err)
		}
	})
}

// Pi's modern OpenAI provider shares its Responses URL between API-key and
// ChatGPT OAuth auth. The separate legacy Codex provider has a different path.
func TestUpstreamResponsesURLs(t *testing.T) {
	cases := []struct {
		name, base, sse, ws string
	}{
		{"chatgpt subscription", "https://api.openai.com/v1", "https://api.openai.com/v1/responses", "wss://api.openai.com/v1/responses"},
		{"complete responses URL", "https://api.openai.com/v1/responses/", "https://api.openai.com/v1/responses", "wss://api.openai.com/v1/responses"},
		{"legacy codex", "https://chatgpt.com/backend-api", "https://chatgpt.com/backend-api/codex/responses", "wss://chatgpt.com/backend-api/codex/responses"},
		{"legacy codex prefix", "https://chatgpt.com/backend-api/codex/", "https://chatgpt.com/backend-api/codex/responses", "wss://chatgpt.com/backend-api/codex/responses"},
		{"complete codex URL", "https://chatgpt.com/backend-api/codex/responses/", "https://chatgpt.com/backend-api/codex/responses", "wss://chatgpt.com/backend-api/codex/responses"},
		{"local proxy", " http://127.0.0.1:8318/v1/ ", "http://127.0.0.1:8318/v1/responses", "ws://127.0.0.1:8318/v1/responses"},
		{"query belongs after path", "https://gateway.example/v1/?route=/blue/", "https://gateway.example/v1/responses?route=/blue/", "wss://gateway.example/v1/responses?route=/blue/"},
		{"complete URL with query", "https://gateway.example/v1/responses?route=blue", "https://gateway.example/v1/responses?route=blue", "wss://gateway.example/v1/responses?route=blue"},
		{"legacy query", "https://gateway.example/backend-api/?route=blue", "https://gateway.example/backend-api/codex/responses?route=blue", "wss://gateway.example/backend-api/codex/responses?route=blue"},
		{"escaped proxy prefix", "https://gateway.example/tenant%2Fone/v1/?route=blue", "https://gateway.example/tenant%2Fone/v1/responses?route=blue", "wss://gateway.example/tenant%2Fone/v1/responses?route=blue"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := Default()
			cfg.Upstream.BaseURL = tc.base
			cfg.normalize()
			if got := cfg.UpstreamSSEURL(); got != tc.sse {
				t.Errorf("SSE URL = %q, want %q", got, tc.sse)
			}
			if got := cfg.UpstreamWSURL(); got != tc.ws {
				t.Errorf("WebSocket URL = %q, want %q", got, tc.ws)
			}
		})
	}
}

func TestDefaultAndEmptyUpstreamUseChatGPTDirectRoute(t *testing.T) {
	for _, empty := range []bool{false, true} {
		cfg := Default()
		if empty {
			cfg.Upstream.BaseURL = "  "
			cfg.Upstream.Transport = ""
			cfg.normalize()
		}
		if cfg.Upstream.BaseURL != "https://api.openai.com/v1" || cfg.UpstreamSSEURL() != "https://api.openai.com/v1/responses" {
			t.Fatalf("default must use Pi's modern ChatGPT subscription provider: %q", cfg.Upstream.BaseURL)
		}
		if cfg.Upstream.Transport != "sse" || cfg.Upstream.SSEZstd {
			t.Fatalf("default must retain uncompressed modern Responses SSE: %+v", cfg.Upstream)
		}
	}
}

func TestUpstreamWSPathRemainsRelativeToBasePath(t *testing.T) {
	for _, path := range []string{"responses", "/responses"} {
		cfg := Default()
		cfg.Upstream.BaseURL = "https://gateway.example/tenant%2Fone/v1/?route=/blue/"
		cfg.Upstream.WSPath = path
		cfg.normalize()
		if got, want := cfg.UpstreamWSURL(), "wss://gateway.example/tenant%2Fone/v1/responses?route=/blue/"; got != want {
			t.Fatalf("WS path %q produced %q, want %q", path, got, want)
		}
	}
}

func TestLoadUpstreamStainlessPlatformAndExplicitTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`upstream:
  stainless_os: MacOS
  stainless_arch: arm64
  stainless_runtime: node
  stainless_runtime_version: v22.14.0
  request_timeout_seconds: 45
`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.StainlessOS != "MacOS" || cfg.Upstream.StainlessArch != "arm64" ||
		cfg.Upstream.StainlessRuntime != "node" || cfg.Upstream.StainlessRuntimeVersion != "v22.14.0" ||
		cfg.Upstream.RequestTimeoutSeconds != 45 {
		t.Fatalf("configured platform and timeout not preserved: %+v", cfg.Upstream)
	}
}

func TestNormalizeUpstreamPlatformDoesNotInventTimeout(t *testing.T) {
	cfg := Default()
	cfg.Upstream.StainlessOS = ""
	cfg.Upstream.StainlessArch = ""
	cfg.Upstream.StainlessRuntime = ""
	cfg.Upstream.StainlessRuntimeVersion = ""
	cfg.Upstream.RequestTimeoutSeconds = -1
	cfg.normalize()
	if cfg.Upstream.StainlessOS != "Linux" || cfg.Upstream.StainlessArch != "x64" ||
		cfg.Upstream.StainlessRuntime != "node" || cfg.Upstream.StainlessRuntimeVersion != "v24.21.0" ||
		cfg.Upstream.RequestTimeoutSeconds != -1 {
		t.Fatalf("normalization = %+v", cfg.Upstream)
	}
}

func TestDefaultUpstreamStainlessPlatform(t *testing.T) {
	cfg := Default()
	if cfg.Upstream.StainlessOS != "Linux" || cfg.Upstream.StainlessArch != "x64" ||
		cfg.Upstream.StainlessRuntime != "node" || cfg.Upstream.StainlessRuntimeVersion != "v24.21.0" {
		t.Fatalf("default platform = %+v, want Linux/x64/node/v20.11.0", cfg.Upstream)
	}
	if cfg.Upstream.RequestTimeoutSeconds != 0 {
		t.Fatalf("default request timeout = %d, want 0", cfg.Upstream.RequestTimeoutSeconds)
	}
}

func TestNormalizeStainlessNames(t *testing.T) {
	platforms := map[string]string{
		"linux":       "Linux",
		"Linux":       "Linux",
		"darwin":      "MacOS",
		"MacOS":       "MacOS",
		"win32":       "Windows",
		"Windows":     "Windows",
		"freebsd":     "FreeBSD",
		"Other:weird": "Other:weird",
		"weird":       "Other:weird",
	}
	for in, want := range platforms {
		if got := normalizeStainlessOS(in); got != want {
			t.Errorf("normalizeStainlessOS(%q) = %q, want %q", in, got, want)
		}
	}
	archs := map[string]string{
		"amd64":      "x64",
		"x86_64":     "x64",
		"x64":        "x64",
		"aarch64":    "arm64",
		"arm64":      "arm64",
		"arm":        "arm",
		"x32":        "x32",
		"ia32":       "other:ia32",
		"other:ia32": "other:ia32",
	}
	for in, want := range archs {
		if got := normalizeStainlessArch(in); got != want {
			t.Errorf("normalizeStainlessArch(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestLoadNormalizesRawStainlessPlatform covers operators pasting Go/Node raw
// values into the config: they must reach the wire as the SDK's canonical names.
func TestLoadNormalizesRawStainlessPlatform(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	body := "upstream:\n  stainless_os: darwin\n  stainless_arch: aarch64\n  stainless_runtime: Node\n"
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.StainlessOS != "MacOS" || cfg.Upstream.StainlessArch != "arm64" ||
		cfg.Upstream.StainlessRuntime != "node" {
		t.Fatalf("raw platform not normalized: %+v", cfg.Upstream)
	}
}

// TestEnvOverridesStainlessPlatform covers the container-friendly path: the
// environment wins over the YAML file and still goes through normalization.
func TestEnvOverridesStainlessPlatform(t *testing.T) {
	t.Setenv("PI_GATEWAY_UPSTREAM_STAINLESS_RUNTIME_VERSION", "v25.1.0")
	t.Setenv("PI_GATEWAY_UPSTREAM_STAINLESS_OS", "darwin")
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("upstream:\n  stainless_runtime_version: v1.2.3\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Upstream.StainlessRuntimeVersion != "v25.1.0" {
		t.Fatalf("env should win over YAML, got %q", cfg.Upstream.StainlessRuntimeVersion)
	}
	if cfg.Upstream.StainlessOS != "MacOS" {
		t.Fatalf("env OS should be normalized to MacOS, got %q", cfg.Upstream.StainlessOS)
	}
}
