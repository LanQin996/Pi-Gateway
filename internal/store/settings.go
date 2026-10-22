package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"pi-gateway/internal/compactprompt"
)

// Settings holds the runtime-adjustable knobs that override config defaults.
type Settings struct {
	SwitchOn429      bool   `json:"switch_on_429"`
	CompactionMode   string `json:"compaction_mode"`
	CompactionModel  string `json:"compaction_model"`
	CompactionPrompt string `json:"compaction_prompt"`

	UpstreamTransport             string            `json:"upstream_transport"`
	CaptureEnabled                bool              `json:"capture_enabled"`
	CaptureLimit                  int               `json:"capture_limit"`
	DefaultModel                  string            `json:"default_model"`
	ModelMappings                 map[string]string `json:"model_mappings"`
	DefaultStrategy               string            `json:"default_strategy"`
	MaxPerAccount                 int               `json:"max_concurrent_per_account"`
	RefreshMarginSecs             int               `json:"refresh_margin_seconds"`
	ReasoningEffort               string            `json:"reasoning_default_effort"`
	ReasoningSummary              string            `json:"reasoning_default_summary"`
	DefaultProxyURL               string            `json:"default_proxy_url"`
	UserAgent                     string            `json:"user_agent"`
	RotationStrategy              string            `json:"rotation_strategy"`
	RequestIntervalMS             int               `json:"request_interval_ms"`
	MaxWaitingPerKey              int               `json:"max_waiting_per_key"`
	MaxWaitingPerAccount          int               `json:"max_waiting_per_account"`
	ConcurrencyWaitTimeoutSeconds int               `json:"concurrency_wait_timeout_seconds"`
	AccountCooldownSeconds        int               `json:"account_cooldown_seconds"`
	MaxAttempts                   int               `json:"max_attempts"`
	UsageRetentionDays            int               `json:"usage_retention_days"`
	SmartSchedulingJSON           string            `json:"smart_scheduling_json"`
	PricingOverridesJSON          string            `json:"pricing_overrides_json"`
}

const (
	settingPrefix          = "settings."
	DefaultCompactionMode  = "on"
	DefaultCompactionModel = "gpt-6-luna"
)

// LoadSettings reads the runtime settings row, falling back to the supplied defaults.
func (s *Store) LoadSettings(ctx context.Context, defaults *Settings) (*Settings, error) {
	var out Settings
	if defaults != nil {
		out = *defaults
	}
	out.applyDefaults()
	rows, err := s.QueryContext(ctx, `SELECT key, value FROM settings WHERE key LIKE ?`, settingPrefix+"%")
	if err != nil {
		return nil, fmt.Errorf("store: load settings: %w", err)
	}
	defer rows.Close()

	values := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, fmt.Errorf("store: scan setting: %w", err)
		}
		values[strings.TrimPrefix(k, settingPrefix)] = v
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out.applyValues(values)
	return &out, nil
}

// SaveSettings writes the runtime settings row.
func (s *Store) SaveSettings(ctx context.Context, in *Settings) error {
	mappings, err := jsonMarshal(in.ModelMappings)
	if err != nil {
		return fmt.Errorf("store: encode model mappings: %w", err)
	}
	pairs := map[string]string{
		"compaction_mode":                  in.CompactionMode,
		"compaction_model":                 in.CompactionModel,
		"compaction_prompt":                in.CompactionPrompt,
		"switch_on_429":                    fmt.Sprintf("%t", in.SwitchOn429),
		"upstream_transport":               in.UpstreamTransport,
		"capture_enabled":                  fmt.Sprintf("%t", in.CaptureEnabled),
		"capture_limit":                    fmt.Sprintf("%d", in.CaptureLimit),
		"default_model":                    in.DefaultModel,
		"model_mappings":                   mappings,
		"default_strategy":                 in.DefaultStrategy,
		"max_concurrent_per_account":       fmt.Sprintf("%d", in.MaxPerAccount),
		"refresh_margin_seconds":           fmt.Sprintf("%d", in.RefreshMarginSecs),
		"reasoning_default_effort":         in.ReasoningEffort,
		"reasoning_default_summary":        in.ReasoningSummary,
		"default_proxy_url":                in.DefaultProxyURL,
		"user_agent":                       in.UserAgent,
		"rotation_strategy":                in.RotationStrategy,
		"request_interval_ms":              fmt.Sprintf("%d", in.RequestIntervalMS),
		"max_waiting_per_key":              fmt.Sprintf("%d", in.MaxWaitingPerKey),
		"max_waiting_per_account":          fmt.Sprintf("%d", in.MaxWaitingPerAccount),
		"concurrency_wait_timeout_seconds": fmt.Sprintf("%d", in.ConcurrencyWaitTimeoutSeconds),
		"account_cooldown_seconds":         fmt.Sprintf("%d", in.AccountCooldownSeconds),
		"max_attempts":                     fmt.Sprintf("%d", in.MaxAttempts),
		"usage_retention_days":             fmt.Sprintf("%d", in.UsageRetentionDays),
		"smart_scheduling_json":            in.SmartSchedulingJSON,
		"pricing_overrides_json":           in.PricingOverridesJSON,
	}
	now := NowMS()
	tx, err := s.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store: begin settings tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	for k, v := range pairs {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO settings (key, value, updated_at) VALUES (?,?,?)
			 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
			settingPrefix+k, v, now); err != nil {
			return fmt.Errorf("store: save setting %s: %w", k, err)
		}
	}
	return tx.Commit()
}

// GetSecret reads an internal (non-user-facing) settings value.
func (s *Store) GetSecret(ctx context.Context, key string) (string, error) {
	var v string
	err := s.QueryRowContext(ctx, `SELECT value FROM settings WHERE key=?`, key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("store: get secret %s: %w", key, err)
	}
	return v, nil
}

// SetSecret writes an internal settings value.
func (s *Store) SetSecret(ctx context.Context, key, value string) error {
	_, err := s.ExecContext(ctx,
		`INSERT INTO settings (key, value, updated_at) VALUES (?,?,?)
		 ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`,
		key, value, NowMS())
	if err != nil {
		return fmt.Errorf("store: set secret %s: %w", key, err)
	}
	return nil
}

// RecordAudit appends an audit event.
func (s *Store) RecordAudit(ctx context.Context, action, detail string) error {
	if _, err := s.ExecContext(ctx, `INSERT INTO audit_events (action, detail, created_at) VALUES (?,?,?)`,
		action, detail, NowMS()); err != nil {
		return fmt.Errorf("store: record audit: %w", err)
	}
	return nil
}

// AuditEvent is a single administrative action record.
type AuditEvent struct {
	ID        int64  `json:"id"`
	Action    string `json:"action"`
	Detail    string `json:"detail"`
	CreatedAt int64  `json:"created_at"`
}

// ListAudit returns the most recent audit events.
func (s *Store) ListAudit(ctx context.Context, limit int) ([]*AuditEvent, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.QueryContext(ctx,
		`SELECT id, action, detail, created_at FROM audit_events ORDER BY id DESC LIMIT ?`, limit)
	if err != nil {
		return nil, fmt.Errorf("store: list audit: %w", err)
	}
	defer rows.Close()

	out := []*AuditEvent{}
	for rows.Next() {
		var e AuditEvent
		if err := rows.Scan(&e.ID, &e.Action, &e.Detail, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan audit: %w", err)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

func (out *Settings) applyDefaults() {
	out.SwitchOn429 = true
	for field, fallback := range map[*string]string{
		&out.CompactionMode:       DefaultCompactionMode,
		&out.CompactionModel:      DefaultCompactionModel,
		&out.RotationStrategy:     "smart",
		&out.SmartSchedulingJSON:  "{}",
		&out.PricingOverridesJSON: "{}",
	} {
		if *field == "" {
			*field = fallback
		}
	}
	if strings.TrimSpace(out.CompactionPrompt) == "" {
		out.CompactionPrompt = compactprompt.Default
	}
	for field, fallback := range map[*int]int{
		&out.ConcurrencyWaitTimeoutSeconds: 30,
		&out.AccountCooldownSeconds:        60,
		&out.MaxAttempts:                   2,
		&out.UsageRetentionDays:            31,
	} {
		if *field == 0 {
			*field = fallback
		}
	}
}

func (out *Settings) applyValues(values map[string]string) {
	out.applyStringValues(values)
	for name, field := range map[string]*bool{
		"switch_on_429":   &out.SwitchOn429,
		"capture_enabled": &out.CaptureEnabled,
	} {
		if value, exists := values[name]; exists {
			*field = value == "true" || value == "1"
		}
	}
	for name, field := range map[string]*int{
		"capture_limit":                    &out.CaptureLimit,
		"max_concurrent_per_account":       &out.MaxPerAccount,
		"refresh_margin_seconds":           &out.RefreshMarginSecs,
		"request_interval_ms":              &out.RequestIntervalMS,
		"max_waiting_per_key":              &out.MaxWaitingPerKey,
		"max_waiting_per_account":          &out.MaxWaitingPerAccount,
		"concurrency_wait_timeout_seconds": &out.ConcurrencyWaitTimeoutSeconds,
		"account_cooldown_seconds":         &out.AccountCooldownSeconds,
		"max_attempts":                     &out.MaxAttempts,
		"usage_retention_days":             &out.UsageRetentionDays,
	} {
		if value, exists := values[name]; exists {
			var parsed int
			if _, err := fmt.Sscanf(value, "%d", &parsed); err == nil {
				*field = parsed
			}
		}
	}
	if value := values["model_mappings"]; value != "" {
		mappings := map[string]string{}
		if err := jsonUnmarshal(value, &mappings); err == nil {
			out.ModelMappings = mappings
		}
	}
}

func (out *Settings) applyStringValues(values map[string]string) {
	for name, field := range map[string]*string{
		"upstream_transport":     &out.UpstreamTransport,
		"compaction_mode":        &out.CompactionMode,
		"compaction_model":       &out.CompactionModel,
		"default_strategy":       &out.DefaultStrategy,
		"rotation_strategy":      &out.RotationStrategy,
		"smart_scheduling_json":  &out.SmartSchedulingJSON,
		"pricing_overrides_json": &out.PricingOverridesJSON,
	} {
		if value := values[name]; value != "" {
			*field = value
		}
	}
	for name, field := range map[string]*string{
		"default_model":             &out.DefaultModel,
		"reasoning_default_effort":  &out.ReasoningEffort,
		"reasoning_default_summary": &out.ReasoningSummary,
		"default_proxy_url":         &out.DefaultProxyURL,
		"user_agent":                &out.UserAgent,
	} {
		if value, exists := values[name]; exists {
			*field = value
		}
	}
	if value := values["compaction_prompt"]; strings.TrimSpace(value) != "" {
		out.CompactionPrompt = value
	}
}
