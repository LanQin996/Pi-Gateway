// Package config loads and validates the gateway configuration.
package config

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
	"gopkg.in/yaml.v3"
)

// Config is the root configuration, loaded from YAML with environment overrides.
type Config struct {
	SchemaVersion int    `yaml:"schema_version"`
	Timezone      string `yaml:"timezone"`

	Server    ServerConfig    `yaml:"server"`
	Admin     AdminConfig     `yaml:"admin"`
	Data      DataConfig      `yaml:"data"`
	Redis     RedisConfig     `yaml:"redis"`
	Upstream  UpstreamConfig  `yaml:"upstream"`
	Accounts  AccountsConfig  `yaml:"accounts"`
	Capture   CaptureConfig   `yaml:"capture"`
	OAuth     OAuthConfig     `yaml:"oauth"`
	Reasoning ReasoningConfig `yaml:"reasoning"`
	Models    ModelsConfig    `yaml:"models"`
	Logging   LoggingConfig   `yaml:"logging"`
}

type ServerConfig struct {
	Host string `yaml:"host"`
	Port int    `yaml:"port"`
	// PublicURL is advertised to the admin UI for convenience (optional).
	PublicURL string `yaml:"public_url"`
}

type AdminConfig struct {
	Username         string `yaml:"username"`
	Password         string `yaml:"password"`
	SessionTTLMinute int    `yaml:"session_ttl_minutes"`
}

type DataConfig struct {
	// Database preserves the legacy SQLite file configuration.
	Database               string `yaml:"database"`
	Driver                 string `yaml:"driver"`
	DSN                    string `yaml:"dsn"`
	MaxOpenConns           int    `yaml:"max_open_conns"`
	MaxIdleConns           int    `yaml:"max_idle_conns"`
	ConnMaxLifetimeSeconds int    `yaml:"conn_max_lifetime_seconds"`
}

// RedisConfig controls bounded, non-authoritative session metadata storage.
// Its limits also apply to the in-memory backend when Redis is disabled.
type RedisConfig struct {
	Enabled           bool   `yaml:"enabled"`
	URL               string `yaml:"url"`
	Password          string `yaml:"password"`
	TLS               bool   `yaml:"tls"`
	Prefix            string `yaml:"prefix"`
	SessionTTLSeconds int    `yaml:"session_ttl_seconds"`
	CommitTimeoutMS   int    `yaml:"commit_timeout_ms"`
	MaxEntries        int    `yaml:"max_entries"`
	MaxRecordBytes    int    `yaml:"max_record_bytes"`
	MaxBytes          int64  `yaml:"max_bytes"`
	CatalogTTLSeconds int    `yaml:"catalog_ttl_seconds"`
}

// DefaultUpstreamBaseURL matches Pi's openai provider, whose API-key and modern
// ChatGPT OAuth authentication share the same Responses endpoint. The separate
// openai-codex provider and Codex quota endpoints use chatgpt.com/backend-api.
const DefaultUpstreamBaseURL = "https://api.openai.com/v1"

type UpstreamConfig struct {
	// BaseURL is the modern Sign in with ChatGPT model endpoint, not the Codex
	// quota endpoint. Changing a URL does not convert one OAuth grant to the other.
	BaseURL string `yaml:"base_url"`

	// WSPath overrides the derived suffix, relative to BaseURL's path (a leading
	// slash is optional). Empty derives it the same way the SSE path is derived.
	WSPath string `yaml:"ws_path"`

	// Transport selects the upstream transport strategy:
	//   passthrough       - mirror the client (client SSE -> upstream SSE, client WS -> upstream WS)
	//   sse               - always use the SSE responses endpoint
	//   websocket         - always open a fresh WebSocket per request
	//   websocket-cached  - reuse a pooled WebSocket and send incremental input
	//   auto              - prefer websocket-cached, fall back to SSE (Pi's default behaviour)
	Transport string `yaml:"transport"`

	// CodexMultiAgentCompat adapts recognized Codex collaboration messages and tools.
	CodexMultiAgentCompat bool `yaml:"codex_multi_agent_compat"`
	// CodexHistoryRecovery repairs replayed encrypted collaboration history only.
	CodexHistoryRecovery bool `yaml:"codex_history_recovery"`

	// SSEZstd compresses SSE request bodies with zstd and sets content-encoding: zstd.
	// It is off by default: the ChatGPT route does not compress its request body.
	SSEZstd bool `yaml:"sse_zstd"`
	// WebsocketPool enables connection reuse for websocket transports.
	WebsocketPool                  bool  `yaml:"websocket_pool"`
	WebsocketMaxBaselineBytes      int64 `yaml:"websocket_max_baseline_bytes"`
	WebsocketMaxTotalBaselineBytes int64 `yaml:"websocket_max_total_baseline_bytes"`
	WebsocketMaxPoolConnections    int   `yaml:"websocket_max_pool_connections"`

	ConnectTimeoutSeconds int `yaml:"connect_timeout_seconds"`
	IdleTimeoutSeconds    int `yaml:"idle_timeout_seconds"`
	// RequestTimeoutSeconds explicitly advertises the caller's SDK timeout.
	// Values <= 0 omit X-Stainless-Timeout; no SDK default is advertised.
	RequestTimeoutSeconds int `yaml:"request_timeout_seconds"`

	// Stainless metadata must match the actual Pi runtime environment, not the Go host.
	StainlessOS             string `yaml:"stainless_os"`
	StainlessArch           string `yaml:"stainless_arch"`
	StainlessRuntime        string `yaml:"stainless_runtime"`
	StainlessRuntimeVersion string `yaml:"stainless_runtime_version"`

	// UserAgent overrides the simulated Pi User-Agent. Empty means auto-detect.
	UserAgent string `yaml:"user_agent"`
	// Originator mirrors Pi's originator header.
	Originator string `yaml:"originator"`
}

type AccountsConfig struct {
	RefreshMarginSeconds    int    `yaml:"refresh_margin_seconds"`
	RefreshIntervalSeconds  int    `yaml:"refresh_interval_seconds"`
	RefreshConcurrency      int    `yaml:"refresh_concurrency"`
	MaxConcurrentPerAccount int    `yaml:"max_concurrent_per_account"`
	DefaultStrategy         string `yaml:"default_strategy"`
	// DefaultProxyURL applies to newly created accounts ("" = direct).
	DefaultProxyURL string `yaml:"default_proxy_url"`
	// QuotaRefreshSeconds is how often usage/rate-limit state is polled per
	// account. 0 disables the background poll (manual refresh still works).
	QuotaRefreshSeconds int `yaml:"quota_refresh_seconds"`
}

type CaptureConfig struct {
	Enabled           bool  `yaml:"enabled"`
	PerAccountLimit   int   `yaml:"per_account_limit"`
	MaxBytesPerRecord int64 `yaml:"max_bytes_per_record"`
	IncludeHeaders    bool  `yaml:"include_headers"`
	// Persist writes captures to SQLite so the web UI can browse them across restarts.
	Persist bool `yaml:"persist"`
}

type OAuthConfig struct {
	CallbackHost   string `yaml:"callback_host"`
	CallbackPort   int    `yaml:"callback_port"`
	TimeoutSeconds int    `yaml:"flow_timeout_seconds"`
}

type ReasoningConfig struct {
	// DefaultEffort is sent as reasoning.effort when the client omits reasoning. "" = omit.
	DefaultEffort string `yaml:"default_effort"`
	// DefaultSummary is sent as reasoning.summary. "" = omit.
	DefaultSummary string `yaml:"default_summary"`
}

type ModelsConfig struct {
	// DefaultModel is used when the client omits the model.
	DefaultModel string `yaml:"default_model"`
	// RefreshIntervalSeconds controls automatic catalog discovery. 0 disables it.
	RefreshIntervalSeconds int `yaml:"refresh_interval_seconds"`
	// Mappings rewrites requested model ids before they reach the upstream.
	Mappings map[string]string `yaml:"mappings"`
	// CodexBaseURL controls only catalog discovery for linked Codex accounts.
	// Generation continues to use Upstream.BaseURL and the main credentials.
	CodexBaseURL string `yaml:"codex_base_url"`
	// CodexClientVersion negotiates the Codex catalog and its client headers.
	// Auto or empty resolves the current stable release; explicit versions stay fixed.
	CodexClientVersion string `yaml:"codex_client_version"`
}

type LoggingConfig struct {
	Level string `yaml:"level"`
	File  string `yaml:"file"`
}

const loopbackHost = "127.0.0.1"

const otherModelPrefix = "Other:"

// Default returns the built-in configuration.
func Default() *Config {
	return &Config{
		SchemaVersion: 1,
		Timezone:      "Asia/Shanghai",
		Server: ServerConfig{
			Host: loopbackHost,
			Port: 8317,
		},
		Admin: AdminConfig{
			Username:         "admin",
			SessionTTLMinute: 1440,
		},
		Data: DataConfig{
			Driver:       "sqlite",
			Database:     filepath.Join("data", "pi-gateway.db"),
			MaxOpenConns: 8,
			MaxIdleConns: 8,
		},
		Redis: RedisConfig{
			Prefix:            "pi-gateway:",
			SessionTTLSeconds: 14400,
			CommitTimeoutMS:   100,
			MaxEntries:        10000,
			MaxRecordBytes:    64 << 10,
			MaxBytes:          64 << 20,
			CatalogTTLSeconds: 30,
		},
		Upstream: UpstreamConfig{
			BaseURL:                        DefaultUpstreamBaseURL,
			Transport:                      "sse",
			CodexMultiAgentCompat:          true,
			CodexHistoryRecovery:           true,
			SSEZstd:                        false,
			WebsocketPool:                  true,
			WebsocketMaxBaselineBytes:      2 << 20,
			WebsocketMaxTotalBaselineBytes: 64 << 20,
			WebsocketMaxPoolConnections:    256,
			ConnectTimeoutSeconds:          15,
			IdleTimeoutSeconds:             300,
			Originator:                     "pi",
			StainlessOS:                    "Linux",
			StainlessArch:                  "x64",
			StainlessRuntime:               "node",
			StainlessRuntimeVersion:        "v24.21.0",
		},
		Accounts: AccountsConfig{
			RefreshMarginSeconds:    3600,
			RefreshIntervalSeconds:  30,
			RefreshConcurrency:      2,
			MaxConcurrentPerAccount: 3,
			DefaultStrategy:         "round_robin",
			QuotaRefreshSeconds:     600,
		},
		Capture: CaptureConfig{
			Enabled:           true,
			PerAccountLimit:   50,
			MaxBytesPerRecord: 64 << 20,
			IncludeHeaders:    true,
			Persist:           true,
		},
		OAuth: OAuthConfig{
			CallbackHost:   loopbackHost,
			CallbackPort:   1455,
			TimeoutSeconds: 300,
		},
		Reasoning: ReasoningConfig{
			DefaultSummary: "auto",
		},
		Models: ModelsConfig{
			RefreshIntervalSeconds: 3600,
			Mappings:               map[string]string{},
			CodexBaseURL:           DefaultCodexBaseURL,
			CodexClientVersion:     AutoCodexClientVersion,
		},
		Logging: LoggingConfig{Level: "info"},
	}
}

// Load reads the config file (if it exists) and applies environment overrides.
func Load(path string) (*Config, error) {
	cfg := Default()

	if path != "" {
		raw, err := os.ReadFile(path)
		switch {
		case err == nil:
			if err := yaml.Unmarshal(raw, cfg); err != nil {
				// YAML errors may echo a DSN/password or a mistyped secret value.
				return nil, errors.New("parse config: invalid YAML or field type (values redacted)")
			}
		case errors.Is(err, os.ErrNotExist):
			// Missing config is fine: defaults apply.
		default:
			return nil, fmt.Errorf("read config %s: %w", path, err)
		}
	}

	if err := cfg.applyEnv(); err != nil {
		return nil, err
	}
	cfg.normalize()

	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

func (c *Config) applyEnv() error {
	if err := c.applyLimitEnv(); err != nil {
		return err
	}
	if v := os.Getenv("PI_GATEWAY_HOST"); v != "" {
		c.Server.Host = v
	}
	if v := os.Getenv("PI_GATEWAY_DATABASE"); v != "" {
		c.Data.Database = v
	}
	if v := os.Getenv("PI_GATEWAY_DATABASE_DRIVER"); v != "" {
		c.Data.Driver = v
	}
	if v, ok := os.LookupEnv("PI_GATEWAY_DATABASE_DSN"); ok {
		c.Data.DSN = v
	}
	c.applyUpstreamEnv()
	if v := os.Getenv("PI_GATEWAY_ADMIN_PASSWORD"); v != "" {
		c.Admin.Password = v
	}
	if v := os.Getenv("PI_GATEWAY_LOG_LEVEL"); v != "" {
		c.Logging.Level = v
	}
	if v := os.Getenv("TZ"); v != "" && c.Timezone == "" {
		c.Timezone = v
	}
	for name, dst := range map[string]*string{
		"PI_GATEWAY_REDIS_URL":            &c.Redis.URL,
		"PI_GATEWAY_REDIS_PASSWORD":       &c.Redis.Password,
		"PI_GATEWAY_REDIS_PREFIX":         &c.Redis.Prefix,
		"PI_GATEWAY_CODEX_BASE_URL":       &c.Models.CodexBaseURL,
		"PI_GATEWAY_CODEX_CLIENT_VERSION": &c.Models.CodexClientVersion,
	} {
		if v, ok := os.LookupEnv(name); ok {
			*dst = v
		}
	}
	return nil
}

// Parse errors deliberately name only the field, never its supplied value.
func (c *Config) applyLimitEnv() error {
	for name, dst := range map[string]*bool{
		"PI_GATEWAY_REDIS_ENABLED":                     &c.Redis.Enabled,
		"PI_GATEWAY_REDIS_TLS":                         &c.Redis.TLS,
		"PI_GATEWAY_UPSTREAM_CODEX_MULTI_AGENT_COMPAT": &c.Upstream.CodexMultiAgentCompat,
		"PI_GATEWAY_UPSTREAM_CODEX_HISTORY_RECOVERY":   &c.Upstream.CodexHistoryRecovery,
	} {
		if v, ok := os.LookupEnv(name); ok {
			n, err := strconv.ParseBool(v)
			if err != nil {
				return fmt.Errorf("config: %s must be a boolean (value redacted)", name)
			}
			*dst = n
		}
	}
	for name, dst := range map[string]*int{
		"PI_GATEWAY_PORT":                                    &c.Server.Port,
		"PI_GATEWAY_DATABASE_MAX_OPEN_CONNS":                 &c.Data.MaxOpenConns,
		"PI_GATEWAY_DATABASE_MAX_IDLE_CONNS":                 &c.Data.MaxIdleConns,
		"PI_GATEWAY_DATABASE_CONN_MAX_LIFETIME_SECONDS":      &c.Data.ConnMaxLifetimeSeconds,
		"PI_GATEWAY_REDIS_SESSION_TTL_SECONDS":               &c.Redis.SessionTTLSeconds,
		"PI_GATEWAY_REDIS_COMMIT_TIMEOUT_MS":                 &c.Redis.CommitTimeoutMS,
		"PI_GATEWAY_REDIS_MAX_ENTRIES":                       &c.Redis.MaxEntries,
		"PI_GATEWAY_REDIS_MAX_RECORD_BYTES":                  &c.Redis.MaxRecordBytes,
		"PI_GATEWAY_REDIS_CATALOG_TTL_SECONDS":               &c.Redis.CatalogTTLSeconds,
		"PI_GATEWAY_MODELS_REFRESH_INTERVAL_SECONDS":         &c.Models.RefreshIntervalSeconds,
		"PI_GATEWAY_UPSTREAM_WEBSOCKET_MAX_POOL_CONNECTIONS": &c.Upstream.WebsocketMaxPoolConnections,
	} {
		if v, ok := os.LookupEnv(name); ok {
			n, err := strconv.Atoi(v)
			if err != nil {
				return fmt.Errorf("config: %s must be an integer (value redacted)", name)
			}
			*dst = n
		}
	}
	for name, dst := range map[string]*int64{
		"PI_GATEWAY_REDIS_MAX_BYTES":                             &c.Redis.MaxBytes,
		"PI_GATEWAY_UPSTREAM_WEBSOCKET_MAX_BASELINE_BYTES":       &c.Upstream.WebsocketMaxBaselineBytes,
		"PI_GATEWAY_UPSTREAM_WEBSOCKET_MAX_TOTAL_BASELINE_BYTES": &c.Upstream.WebsocketMaxTotalBaselineBytes,
	} {
		if v, ok := os.LookupEnv(name); ok {
			n, err := strconv.ParseInt(v, 10, 64)
			if err != nil {
				return fmt.Errorf("config: %s must be an integer (value redacted)", name)
			}
			*dst = n
		}
	}
	return nil
}

func (c *Config) normalize() {
	c.Data.Driver = strings.ToLower(strings.TrimSpace(c.Data.Driver))
	switch c.Data.Driver {
	case "":
		c.Data.Driver = "sqlite"
	case "postgresql", "pgx":
		c.Data.Driver = "postgres"
	}
	if c.SchemaVersion == 0 {
		c.SchemaVersion = 1
	}
	c.normalizeUpstream()
	c.Accounts.DefaultStrategy = strings.ToLower(strings.TrimSpace(c.Accounts.DefaultStrategy))
	if c.Accounts.DefaultStrategy == "" {
		c.Accounts.DefaultStrategy = "round_robin"
	}
	c.Admin.Username = strings.TrimSpace(c.Admin.Username)
	if c.Admin.Username == "" {
		c.Admin.Username = "admin"
	}
	if c.Admin.SessionTTLMinute <= 0 {
		c.Admin.SessionTTLMinute = 1440
	}
	if c.Capture.PerAccountLimit <= 0 {
		c.Capture.PerAccountLimit = 50
	}
	if c.Capture.MaxBytesPerRecord <= 0 {
		c.Capture.MaxBytesPerRecord = 64 << 20
	}
	if c.Models.Mappings == nil {
		c.Models.Mappings = map[string]string{}
	}
	// Only exact zero values use defaults; invalid whitespace must reach validation.
	if c.Models.CodexBaseURL == "" {
		c.Models.CodexBaseURL = DefaultCodexBaseURL
	}
	if c.Models.CodexClientVersion == "" {
		c.Models.CodexClientVersion = AutoCodexClientVersion
	}
	if c.Logging.Level == "" {
		c.Logging.Level = "info"
	}
}

// ValidTransports lists the accepted upstream transport strategies.
var ValidTransports = []string{"passthrough", "sse", "websocket", "websocket-cached", "auto"}

// ValidStrategies lists the accepted account selection strategies.
var ValidStrategies = []string{"smart", "quota_reset_priority", "round_robin", "sticky", "least_inflight", "single"}

// Validate checks the configuration for impossible combinations.
func (c *Config) Validate() error {
	if err := c.validateStorageLimits(); err != nil {
		return err
	}
	if _, err := c.codexModelsURL(); err != nil {
		return err
	}
	if seconds := c.Models.RefreshIntervalSeconds; seconds < 0 || seconds > 0 && seconds < 30 || int64(seconds) > int64((1<<63-1)/time.Second) {
		return errors.New("models.refresh_interval_seconds must be 0 or at least 30 seconds and fit a duration (value redacted)")
	}
	if err := c.validateDatabase(); err != nil {
		return err
	}
	if c.Server.Port <= 0 || c.Server.Port > 65535 {
		return fmt.Errorf("server.port out of range: %d", c.Server.Port)
	}
	if !contains(ValidTransports, c.Upstream.Transport) {
		return fmt.Errorf("upstream.transport %q is invalid (want one of %s)", c.Upstream.Transport, strings.Join(ValidTransports, ", "))
	}
	if !contains(ValidStrategies, c.Accounts.DefaultStrategy) {
		return fmt.Errorf("accounts.default_strategy %q is invalid (want one of %s)", c.Accounts.DefaultStrategy, strings.Join(ValidStrategies, ", "))
	}
	if c.Upstream.ConnectTimeoutSeconds <= 0 {
		c.Upstream.ConnectTimeoutSeconds = 15
	}
	if c.Upstream.IdleTimeoutSeconds <= 0 {
		c.Upstream.IdleTimeoutSeconds = 300
	}
	if c.OAuth.CallbackPort <= 0 || c.OAuth.CallbackPort > 65535 {
		c.OAuth.CallbackPort = 1455
	}
	if c.OAuth.CallbackHost == "" {
		c.OAuth.CallbackHost = loopbackHost
	}
	if c.OAuth.TimeoutSeconds <= 0 {
		c.OAuth.TimeoutSeconds = 300
	}
	if _, err := time.LoadLocation(c.Timezone); err != nil {
		return fmt.Errorf("timezone %q is invalid: %w", c.Timezone, err)
	}
	return nil
}

func (c *Config) validateStorageLimits() error {
	if c.Data.MaxOpenConns < 0 || c.Data.MaxIdleConns < 0 || c.Data.ConnMaxLifetimeSeconds < 0 || int64(c.Data.ConnMaxLifetimeSeconds) > int64((1<<63-1)/time.Second) {
		return errors.New("config: data connection pool limits are invalid (values redacted)")
	}
	if err := c.validateRedisLimits(); err != nil {
		return err
	}
	if c.Upstream.WebsocketMaxBaselineBytes <= 0 || c.Upstream.WebsocketMaxTotalBaselineBytes < c.Upstream.WebsocketMaxBaselineBytes || c.Upstream.WebsocketMaxPoolConnections <= 0 {
		return errors.New("config: upstream websocket capacity limits are invalid (values redacted)")
	}
	return nil
}

// Location returns the configured display timezone.
func (c *Config) Location() *time.Location {
	loc, err := time.LoadLocation(c.Timezone)
	if err != nil {
		return time.UTC
	}
	return loc
}

// Addr is the listen address for the HTTP server.
func (c *Config) Addr() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}

// UpstreamSSEURL is the responses endpoint used for the SSE transport.
//
// Pi's modern ChatGPT subscription provider uses .../v1/responses. Legacy
// Codex paths remain supported for compatible endpoints; selecting their URL
// alone does not switch this gateway's primary ChatGPT credential or wire shape.
func (c *Config) UpstreamSSEURL() string {
	base := normalizeUpstreamBaseURL(c.Upstream.BaseURL)
	parsed, err := url.Parse(base)
	if err != nil {
		return base // The request constructor will report the invalid URL.
	}
	path := parsed.EscapedPath()
	switch {
	case strings.HasSuffix(path, "/responses"):
		return base
	case strings.HasSuffix(path, "/backend-api"):
		return appendUpstreamPath(base, "/codex/responses")
	default:
		return appendUpstreamPath(base, "/responses")
	}
}

// UpstreamWSURL is the WebSocket endpoint. It uses upstream.ws_path as a suffix
// relative to the base path when set, otherwise mirroring the SSE path.
func (c *Config) UpstreamWSURL() string {
	path := strings.TrimSpace(c.Upstream.WSPath)
	if path != "" {
		return toWebSocketScheme(appendUpstreamPath(normalizeUpstreamBaseURL(c.Upstream.BaseURL), path))
	}
	return toWebSocketScheme(c.UpstreamSSEURL())
}

// normalizeUpstreamBaseURL trims path slashes, never trailing query characters.
// Work with the escaped path so a proxy's encoded path segments stay intact.
func normalizeUpstreamBaseURL(raw string) string {
	base := strings.TrimSpace(raw)
	if base == "" {
		return DefaultUpstreamBaseURL
	}
	parsed, err := url.Parse(base)
	if err != nil {
		return base
	}
	parsed.RawPath = strings.TrimRight(parsed.EscapedPath(), "/")
	parsed.Path, _ = url.PathUnescape(parsed.RawPath)
	return parsed.String()
}

func appendUpstreamPath(base, suffix string) string {
	parsed, baseErr := url.Parse(base)
	relative, pathErr := url.Parse("/" + strings.TrimLeft(suffix, "/"))
	if baseErr != nil || pathErr != nil {
		return base + "/" + strings.TrimLeft(suffix, "/")
	}
	parsed.RawPath = strings.TrimRight(parsed.EscapedPath(), "/") + relative.EscapedPath()
	parsed.Path, _ = url.PathUnescape(parsed.RawPath)
	if relative.RawQuery != "" || relative.ForceQuery {
		parsed.RawQuery, parsed.ForceQuery = relative.RawQuery, relative.ForceQuery
	}
	if relative.Fragment != "" {
		parsed.Fragment, parsed.RawFragment = relative.Fragment, relative.RawFragment
	}
	return parsed.String()
}

func toWebSocketScheme(u string) string {
	if strings.HasPrefix(u, "https://") {
		return "wss://" + strings.TrimPrefix(u, "https://")
	}
	if strings.HasPrefix(u, "http://") {
		return "ws://" + strings.TrimPrefix(u, "http://")
	}
	return u
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

// normalizeStainlessOS mirrors normalizePlatform() in openai-node's
// src/internal/detect-platform.ts: raw process.platform values map to the
// canonical names, and anything unrecognized becomes "Other:<lowercased>".
// Already-normalized names (and Other:* values) pass through untouched so the
// mapping stays idempotent for configs that already hold canonical values.
func normalizeStainlessOS(platform string) string {
	trimmed := strings.TrimSpace(platform)
	if trimmed == "" {
		return ""
	}
	switch trimmed {
	case "MacOS", "Windows", "Linux", "iOS", "Android", "FreeBSD", "OpenBSD", "Unknown":
		return trimmed
	}
	if len(trimmed) >= len(otherModelPrefix) && strings.EqualFold(trimmed[:len(otherModelPrefix)], otherModelPrefix) {
		return trimmed
	}
	lowered := strings.ToLower(trimmed)
	switch {
	case strings.Contains(lowered, "ios"):
		return "iOS"
	case lowered == "android":
		return "Android"
	case lowered == "darwin" || lowered == "macos":
		return "MacOS"
	case lowered == "win32" || lowered == "windows":
		return "Windows"
	case lowered == "freebsd":
		return "FreeBSD"
	case lowered == "openbsd":
		return "OpenBSD"
	case lowered == "linux":
		return "Linux"
	default:
		return otherModelPrefix + lowered
	}
}

// normalizeStainlessArch mirrors normalizeArch() in the same SDK file. The SDK
// deliberately does not map ia32, so neither do we; Go's "amd64" is mapped to
// x64 so GOARCH values can be pasted in directly.
func normalizeStainlessArch(arch string) string {
	trimmed := strings.TrimSpace(arch)
	if trimmed == "" {
		return ""
	}
	switch trimmed {
	case "x64", "arm64", "arm", "x32", "unknown":
		return trimmed
	}
	if strings.HasPrefix(trimmed, "other:") {
		return trimmed
	}
	switch strings.ToLower(trimmed) {
	case "x86_64", "x64", "amd64":
		return "x64"
	case "aarch64", "arm64":
		return "arm64"
	case "arm", "armv7l", "armv6l":
		return "arm"
	case "x32":
		return "x32"
	default:
		return "other:" + trimmed
	}
}

func (c *Config) applyUpstreamEnv() {
	if v := os.Getenv("PI_GATEWAY_UPSTREAM_BASE_URL"); v != "" {
		c.Upstream.BaseURL = v
	}
	if v := os.Getenv("PI_GATEWAY_UPSTREAM_TRANSPORT"); v != "" {
		c.Upstream.Transport = v
	}
	if v := os.Getenv("PI_GATEWAY_USER_AGENT"); v != "" {
		c.Upstream.UserAgent = v
	}
	// The SDK always sends a platform fingerprint; these override the built-in
	// defaults so a deployment can match the machine actually running Pi.
	if v := os.Getenv("PI_GATEWAY_UPSTREAM_STAINLESS_OS"); v != "" {
		c.Upstream.StainlessOS = v
	}
	if v := os.Getenv("PI_GATEWAY_UPSTREAM_STAINLESS_ARCH"); v != "" {
		c.Upstream.StainlessArch = v
	}
	if v := os.Getenv("PI_GATEWAY_UPSTREAM_STAINLESS_RUNTIME"); v != "" {
		c.Upstream.StainlessRuntime = v
	}
	if v := os.Getenv("PI_GATEWAY_UPSTREAM_STAINLESS_RUNTIME_VERSION"); v != "" {
		c.Upstream.StainlessRuntimeVersion = v
	}
}

func (c *Config) normalizeUpstream() {
	c.Upstream.BaseURL = normalizeUpstreamBaseURL(c.Upstream.BaseURL)
	c.Upstream.Transport = strings.ToLower(strings.TrimSpace(c.Upstream.Transport))
	if c.Upstream.Transport == "" {
		// Match Default(): the ChatGPT route is SSE, and an unset value must not
		// silently mirror the client's protocol.
		c.Upstream.Transport = "sse"
	}
	if c.Upstream.Originator == "" {
		c.Upstream.Originator = "pi"
	}
	if c.Upstream.StainlessOS == "" {
		c.Upstream.StainlessOS = "Linux"
	}
	if c.Upstream.StainlessArch == "" {
		c.Upstream.StainlessArch = "x64"
	}
	if c.Upstream.StainlessRuntime == "" {
		c.Upstream.StainlessRuntime = "node"
	}
	if c.Upstream.StainlessRuntimeVersion == "" {
		c.Upstream.StainlessRuntimeVersion = "v24.21.0"
	}
	// Operators may supply either the raw runtime values (process.platform /
	// process.arch, or Go's GOOS/GOARCH) or the already-normalized names; apply
	// the openai-node normalization so the wire values match the SDK.
	c.Upstream.StainlessOS = normalizeStainlessOS(c.Upstream.StainlessOS)
	c.Upstream.StainlessArch = normalizeStainlessArch(c.Upstream.StainlessArch)
	c.Upstream.StainlessRuntime = strings.ToLower(strings.TrimSpace(c.Upstream.StainlessRuntime))
	c.Upstream.StainlessRuntimeVersion = strings.TrimSpace(c.Upstream.StainlessRuntimeVersion)
}

func (c *Config) validateDatabase() error {
	switch strings.ToLower(strings.TrimSpace(c.Data.Driver)) {
	case "", "sqlite":
		if strings.TrimSpace(c.Data.Database) == "" && strings.TrimSpace(c.Data.DSN) == "" {
			return errors.New("data.database or data.dsn is required for SQLite")
		}
	case "postgres", "postgresql", "pgx":
		if strings.TrimSpace(c.Data.DSN) == "" {
			return errors.New("data.dsn is required for PostgreSQL")
		}
	default:
		return fmt.Errorf("data.driver %q is invalid (want sqlite or postgres)", c.Data.Driver)
	}
	return nil
}

func (c *Config) validateRedisLimits() error {
	if c.Redis.Enabled || c.Redis.URL != "" {
		u, err := redis.ParseURL(c.Redis.URL)
		if err != nil || u == nil || (!strings.HasPrefix(c.Redis.URL, "redis://") && !strings.HasPrefix(c.Redis.URL, "rediss://")) {
			return errors.New("config: redis.url must be a valid redis:// or rediss:// URL (value redacted)")
		}
	}
	if len(c.Redis.Prefix) > 256 || strings.ContainsAny(c.Redis.Prefix, "\r\n\x00") {
		return errors.New("config: redis.prefix is invalid (value redacted)")
	}
	if err := c.validateRedisDurations(); err != nil {
		return err
	}
	if c.Redis.CommitTimeoutMS <= 0 || int64(c.Redis.CommitTimeoutMS) > int64((1<<63-1)/time.Millisecond) {
		return errors.New("config: redis.commit_timeout_ms must be a positive duration (value redacted)")
	}
	if c.Redis.MaxEntries <= 0 || c.Redis.MaxRecordBytes <= 0 || c.Redis.MaxRecordBytes > 64<<10 || c.Redis.MaxBytes < int64(c.Redis.MaxRecordBytes) {
		return errors.New("config: redis capacity limits are invalid; max_record_bytes must be 1..65536 and max_bytes must cover one record (values redacted)")
	}
	return nil
}

func (c *Config) validateRedisDurations() error {
	for name, value := range map[string]int{
		"redis.session_ttl_seconds": c.Redis.SessionTTLSeconds,
		"redis.catalog_ttl_seconds": c.Redis.CatalogTTLSeconds,
	} {
		if value <= 0 || int64(value) > int64((1<<63-1)/time.Second) {
			return fmt.Errorf("config: %s must be a positive duration (value redacted)", name)
		}
	}
	return nil
}
