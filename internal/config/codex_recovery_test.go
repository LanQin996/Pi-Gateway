package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexHistoryRecoveryConfig(t *testing.T) {
	for _, tc := range []struct {
		name, yaml, env string
		want            bool
	}{
		{"default", "upstream: {}\n", "", true},
		{"strict_yaml", "upstream:\n  codex_history_recovery: false\n", "", false},
		{"strict_env", "upstream: {}\n", "false", false},
		{"enable_env", "upstream:\n  codex_history_recovery: false\n", "true", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("PI_GATEWAY_UPSTREAM_CODEX_HISTORY_RECOVERY", tc.env)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Upstream.CodexHistoryRecovery != tc.want {
				t.Fatal("wrong recovery setting")
			}
		})
	}
	t.Setenv("PI_GATEWAY_UPSTREAM_CODEX_HISTORY_RECOVERY", "private-invalid-value")
	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "PI_GATEWAY_UPSTREAM_CODEX_HISTORY_RECOVERY") || strings.Contains(err.Error(), "private-invalid-value") {
		t.Fatal("invalid setting accepted or leaked")
	}
}
