package config

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestModelsRefreshIntervalConfig(t *testing.T) {
	if got := Default().Models.RefreshIntervalSeconds; got != 3600 {
		t.Fatalf("default interval = %d, want 3600", got)
	}
	for _, tc := range []struct {
		name, yaml, env string
		want            int
	}{
		{"omitted", "models: {}\n", "", 3600},
		{"disabled", "models:\n  refresh_interval_seconds: 0\n", "", 0},
		{"minimum", "models:\n  refresh_interval_seconds: 30\n", "", 30},
		{"env_disabled", "models:\n  refresh_interval_seconds: 600\n", "0", 0},
		{"env_enabled", "models:\n  refresh_interval_seconds: 0\n", "120", 120},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.env != "" {
				t.Setenv("PI_GATEWAY_MODELS_REFRESH_INTERVAL_SECONDS", tc.env)
			}
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(tc.yaml), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.Models.RefreshIntervalSeconds; got != tc.want {
				t.Fatalf("interval = %d, want %d", got, tc.want)
			}
		})
	}
}

func TestModelsRefreshIntervalValidation(t *testing.T) {
	type intervalCase struct {
		seconds int
		valid   bool
	}
	cases := []intervalCase{{-1, false}, {0, true}, {1, false}, {29, false}, {30, true}, {3600, true}}
	if strconv.IntSize == 64 {
		maxSeconds := int64((1<<63 - 1) / time.Second)
		cases = append(cases, intervalCase{int(maxSeconds), true}, intervalCase{int(maxSeconds + 1), false})
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.seconds), func(t *testing.T) {
			cfg := Default()
			cfg.Models.RefreshIntervalSeconds = tc.seconds
			err := cfg.Validate()
			if (err == nil) != tc.valid {
				t.Fatalf("Validate(%d) = %v, valid = %v", tc.seconds, err, tc.valid)
			}
			if err != nil && !strings.Contains(err.Error(), "models.refresh_interval_seconds") {
				t.Fatalf("wrong validation failure: %v", err)
			}
		})
	}
}

func TestModelsRefreshIntervalInvalidEnv(t *testing.T) {
	for _, value := range []string{"private-invalid-value", "999999999999999999999", "-1", "29"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv("PI_GATEWAY_MODELS_REFRESH_INTERVAL_SECONDS", value)
			_, err := Load("")
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), "refresh_interval_seconds") {
				t.Fatalf("invalid interval was accepted or wrong error returned: %v", err)
			}
			if strings.Contains(err.Error(), value) {
				t.Fatalf("invalid value leaked: %v", err)
			}
		})
	}
}
