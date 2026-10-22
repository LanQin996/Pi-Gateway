package config

import (
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestModelCatalogURLUsesConfiguredPrimaryEndpoint(t *testing.T) {
	for _, tt := range []struct{ base, want string }{
		{"", "https://api.openai.com/v1/models"},
		{"https://example.test/v1", "https://example.test/v1/models"},
		{"https://example.test/v1/responses", "https://example.test/v1/models"},
		{"http://localhost:18080/custom/responses/?route=a%2Fb", "http://localhost:18080/custom/models?route=a%2Fb"},
		{"https://example.test/custom%2Fbase/", "https://example.test/custom%2Fbase/models"},
	} {
		cfg := Default()
		cfg.Upstream.BaseURL = tt.base
		if got := cfg.UpstreamModelsURL(); got != tt.want {
			t.Errorf("base=%q got=%q want=%q", tt.base, got, tt.want)
		}
	}
}

func TestCodexClientVersionDefaultsAndModes(t *testing.T) {
	reportTestFailure(t, AutoCodexClientVersion != "auto" || DefaultCodexClientVersion != "0.160.0", func() {
		t.Fatalf("unexpected automatic mode or offline fallback: %q / %q", AutoCodexClientVersion, DefaultCodexClientVersion)
	})
	{
		got := Default().Models.CodexClientVersion
		reportTestFailure(t, got != AutoCodexClientVersion, func() {
			t.Fatalf("default client version = %q, want auto", got)
		})
	}
	for _, tt := range []struct {
		name, version, normalized, wire string
	}{
		{"empty", "", AutoCodexClientVersion, DefaultCodexClientVersion},
		{"auto", AutoCodexClientVersion, AutoCodexClientVersion, DefaultCodexClientVersion},
		{"fixed legacy", "0.153.4", "0.153.4", "0.153.4"},
		{"fixed fallback", "0.160.0", "0.160.0", "0.160.0"},
		{"fixed custom", "v1.2.3-rc_1+build.4", "v1.2.3-rc_1+build.4", "v1.2.3-rc_1+build.4"},
		{"case remains fixed", "AUTO", "AUTO", "AUTO"},
		{"maximum length", strings.Repeat("a", 64), strings.Repeat("a", 64), strings.Repeat("a", 64)},
	} {
		t.Run(tt.name, func(t *testing.T) {
			cfg := Default()
			cfg.Models.CodexClientVersion = tt.version
			for _, normalize := range []bool{false, true} {
				if normalize {
					cfg.normalize()
					{
						got := cfg.Models.CodexClientVersion
						reportTestFailure(t, got != tt.normalized, func() {
							t.Fatalf("normalized version = %q, want %q", got, tt.normalized)
						})
					}
				}
				{
					err := cfg.Validate()
					reportTestFailure(t, err != nil, func() {
						t.Fatal(err)
					})
				}
				before := *cfg
				got := cfg.CodexModelsURL()
				want := "https://chatgpt.com/backend-api/codex/models?client_version=" + url.QueryEscape(tt.wire)
				reportTestFailure(t, got != want, func() {
					t.Errorf("URL = %q, want %q", got, want)
				})
				reportTestFailure(t, !reflect.DeepEqual(*cfg, before) || cfg.CodexModelsURL() != got, func() {
					t.Fatal("CodexModelsURL must be deterministic and must not mutate configuration")
				})
			}
		})
	}
	var zero Config
	{
		got, want := zero.CodexModelsURL(), "https://chatgpt.com/backend-api/codex/models?client_version="+DefaultCodexClientVersion
		reportTestFailure(t, got != want, func() {
			t.Errorf("zero-value URL = %q, want %q", got, want)
		})
	}
	zero.normalize()
	reportTestFailure(t, zero.Models.CodexClientVersion != AutoCodexClientVersion, func() {
		t.Fatalf("zero-value normalization did not select auto: %q", zero.Models.CodexClientVersion)
	})
}

func TestCodexModelsURLWithResolvedConfigCopy(t *testing.T) {
	cfg := Default()
	resolved := *cfg
	resolved.Models.CodexClientVersion = "0.161.0"
	if got, want := resolved.CodexModelsURL(), "https://chatgpt.com/backend-api/codex/models?client_version=0.161.0"; got != want {
		t.Errorf("resolved URL = %q, want %q", got, want)
	}
	if cfg.Models.CodexClientVersion != AutoCodexClientVersion {
		t.Fatalf("resolving a copied configuration modified the original: %q", cfg.Models.CodexClientVersion)
	}
	if cfg.Upstream != resolved.Upstream || cfg.UpstreamSSEURL() != "https://api.openai.com/v1/responses" {
		t.Fatal("Codex version resolution must not change the main upstream")
	}
}

func TestLoadCodexClientVersion(t *testing.T) {
	// Restore every caller-provided value after testing; no subtest runs in parallel.
	for _, pair := range os.Environ() {
		name, _, _ := strings.Cut(pair, "=")
		if strings.HasPrefix(name, "PI_GATEWAY_") {
			t.Setenv(name, "")
			{
				err := os.Unsetenv(name)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
		}
	}
	for _, tt := range []struct {
		name, yaml, version, wire string
		env                       map[string]string
		invalid                   bool
	}{
		{name: "omitted", version: AutoCodexClientVersion, wire: DefaultCodexClientVersion},
		{name: "empty YAML", yaml: "models:\n  codex_client_version: ''\n", version: AutoCodexClientVersion, wire: DefaultCodexClientVersion},
		{name: "auto YAML", yaml: "models:\n  codex_client_version: auto\n", version: AutoCodexClientVersion, wire: DefaultCodexClientVersion},
		{name: "fixed YAML", yaml: "models:\n  codex_client_version: '0.153.4'\n", version: "0.153.4", wire: "0.153.4"},
		{name: "auto environment overrides fixed", yaml: "models:\n  codex_client_version: '0.153.4'\n", env: map[string]string{"PI_GATEWAY_CODEX_CLIENT_VERSION": "auto"}, version: AutoCodexClientVersion, wire: DefaultCodexClientVersion},
		{name: "empty environment overrides fixed", yaml: "models:\n  codex_client_version: '0.153.4'\n", env: map[string]string{"PI_GATEWAY_CODEX_CLIENT_VERSION": ""}, version: AutoCodexClientVersion, wire: DefaultCodexClientVersion},
		{name: "fixed environment overrides auto", yaml: "models:\n  codex_client_version: auto\n", env: map[string]string{"PI_GATEWAY_CODEX_CLIENT_VERSION": "0.153.4"}, version: "0.153.4", wire: "0.153.4"},
		{name: "base and version environment overrides", yaml: "models:\n  codex_base_url: https://yaml.example/backend-api\n  codex_client_version: auto\n", env: map[string]string{"PI_GATEWAY_CODEX_BASE_URL": "http://localhost:18080/backend-api", "PI_GATEWAY_CODEX_CLIENT_VERSION": "v1.2.3+build"}, version: "v1.2.3+build", wire: "v1.2.3+build"},
		{name: "invalid YAML", yaml: "models:\n  codex_client_version: 'sensitive value'\n", invalid: true},
		{name: "invalid environment", yaml: "models:\n  codex_client_version: auto\n", env: map[string]string{"PI_GATEWAY_CODEX_CLIENT_VERSION": "sensitive value"}, invalid: true},
		{name: "whitespace auto environment", env: map[string]string{"PI_GATEWAY_CODEX_CLIENT_VERSION": " auto "}, invalid: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			for key, value := range tt.env {
				t.Setenv(key, value)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			{
				err := os.WriteFile(path, []byte(tt.yaml), 0600)
				reportTestFailure(t, err != nil, func() {
					t.Fatal(err)
				})
			}
			cfg, err := Load(path)
			if tt.invalid {
				reportTestFailure(t, err == nil || !strings.Contains(err.Error(), "models.codex_client_version") || strings.Contains(err.Error(), "sensitive"), func() {
					t.Fatalf("want redacted version validation error, got %v", err)
				})
				return
			}
			reportTestFailure(t, err != nil, func() {
				t.Fatal(err)
			})
			reportTestFailure(t, cfg.Models.CodexClientVersion != tt.version, func() {
				t.Errorf("loaded client version = %q, want %q", cfg.Models.CodexClientVersion, tt.version)
			})
			base := DefaultCodexBaseURL
			if configured, ok := tt.env["PI_GATEWAY_CODEX_BASE_URL"]; ok {
				base = configured
			}
			reportTestFailure(t, cfg.Models.CodexBaseURL != base, func() {
				t.Errorf("loaded base = %q, want %q", cfg.Models.CodexBaseURL, base)
			})
			{
				got, want := cfg.CodexModelsURL(), base+"/codex/models?client_version="+url.QueryEscape(tt.wire)
				reportTestFailure(t, got != want, func() {
					t.Errorf("loaded URL = %q, want %q", got, want)
				})
			}
			reportTestFailure(t, cfg.UpstreamSSEURL() != "https://api.openai.com/v1/responses", func() {
				t.Fatal("Codex environment must not change the main upstream")
			})
		})
	}
}

func TestCodexModelsURLPreservesPathsAndQueries(t *testing.T) {
	for _, version := range []string{"", AutoCodexClientVersion, "v1.2.3-rc_1+build.4"} {
		t.Run("version="+version, func(t *testing.T) {
			wire := version
			if wire == "" || wire == AutoCodexClientVersion {
				wire = DefaultCodexClientVersion
			}
			for _, tt := range []struct{ base, want string }{
				{"", "https://chatgpt.com/backend-api/codex/models"},
				{"https://chatgpt.com", "https://chatgpt.com/backend-api/codex/models"},
				{"https://gateway.example/backend-api/", "https://gateway.example/backend-api/codex/models"},
				{"https://gateway.example/backend-api/codex/", "https://gateway.example/backend-api/codex/models"},
				{"https://gateway.example/backend-api/codex/models/", "https://gateway.example/backend-api/codex/models"},
				{"http://localhost:18080/tenant%2Fone/", "http://localhost:18080/tenant%2Fone/codex/models"},
				{"https://gateway.example/tenant%2Fone/%6dodels", "https://gateway.example/tenant%2Fone/%6dodels"},
				{"http://[::1]:18080", "http://[::1]:18080/backend-api/codex/models"},
			} {
				cfg := Default()
				cfg.Upstream.BaseURL = "https://main.example/v1"
				cfg.Models.CodexBaseURL = tt.base
				cfg.Models.CodexClientVersion = version
				if err := cfg.Validate(); err != nil {
					t.Fatalf("base=%q: %v", tt.base, err)
				}
				if got, want := cfg.CodexModelsURL(), tt.want+"?client_version="+url.QueryEscape(wire); got != want {
					t.Errorf("base=%q URL=%q, want %q", tt.base, got, want)
				}
			}
			cfg := Default()
			cfg.Models.CodexBaseURL = "https://gateway.example/tenant%2Fone/codex/models?route=a%2Fb&label=a%2Bb&route=c&client_version=old&client_version=older"
			cfg.Models.CodexClientVersion = version
			want := "https://gateway.example/tenant%2Fone/codex/models?client_version=" + url.QueryEscape(wire) + "&label=a%2Bb&route=a%2Fb&route=c"
			if got := cfg.CodexModelsURL(); got != want {
				t.Errorf("escaped URL = %q, want %q", got, want)
			}
		})
	}
}

func TestCodexClientVersionRejectsInvalidInput(t *testing.T) {
	for _, version := range []string{
		" ", " auto", "auto ", "auto\n", "\tauto", "0.160.0\r\nAuthorization: sensitive",
		"v1 version", ".1", "_1", "+1", "-1", "v1/2", "v1\\2", "v1?x", "v1&token=sensitive",
		"v1#fragment", "v1%20", "版本", "v1\x00", "v1\x7f", strings.Repeat("a", 65),
	} {
		t.Run(version, func(t *testing.T) {
			cfg := Default()
			cfg.Models.CodexClientVersion = version
			cfg.normalize()
			if cfg.Models.CodexClientVersion != version {
				t.Fatal("normalization must not sanitize an invalid nonempty version")
			}
			if got := cfg.CodexModelsURL(); got != "" {
				t.Errorf("invalid version produced URL %q", got)
			}
			err := cfg.Validate()
			if err == nil || !strings.HasPrefix(err.Error(), "models.codex_client_version:") || strings.Contains(err.Error(), "sensitive") {
				t.Fatalf("want redacted version validation error, got %v", err)
			}
		})
	}
}

func TestCodexModelsURLRejectsUnsafeURLs(t *testing.T) {
	bases := []string{
		" https://gateway.example", "https://gateway.example ", "ftp://gateway.example",
		"/backend-api", "https:", "https:opaque", "https://", "https://gateway.example:%",
		"https://gateway.example:0", "https://gateway.example:65536", "https://gateway.example:",
		"https://bad_host.example", "https://-bad.example", "https://bad-.example", "https://bad..example",
		"https://user:sensitive@gateway.example", "https://gateway.example/#sensitive", "https://gateway.example/#",
		"https://api.openai.com", "https://API.OPENAI.COM.:443/backend-api",
		"https://gateway.example/%0d%0a", "https://gateway.example/?route=%00",
		"https://gateway.example/?%0d=sensitive", "https://gateway.example/?route=%xx",
		"https://gateway.example/?route=sensitive;other=value",
	}
	for _, key := range []string{"token", "access_token", "refresh_token", "id_token", "api_key", "apikey", "key", "authorization", "auth", "bearer", "password", "client_secret", "secret", "Access-Token", "%61pi_key"} {
		bases = append(bases, "https://gateway.example/?"+key+"=sensitive")
	}
	for _, version := range []string{"", AutoCodexClientVersion, "0.153.4"} {
		t.Run("version="+version, func(t *testing.T) {
			for _, base := range bases {
				cfg := Default()
				cfg.Models.CodexBaseURL = base
				cfg.Models.CodexClientVersion = version
				if got := cfg.CodexModelsURL(); got != "" {
					t.Errorf("unsafe base %q produced URL %q", base, got)
				}
				err := cfg.Validate()
				if err == nil || !strings.HasPrefix(err.Error(), "models.codex_base_url:") || strings.Contains(err.Error(), "sensitive") {
					t.Errorf("base %q: want redacted URL validation error, got %v", base, err)
				}
			}
		})
	}
}
