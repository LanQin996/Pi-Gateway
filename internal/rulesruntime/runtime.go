// Package rulesruntime connects the pure rule language to transactional storage.
package rulesruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"time"

	"pi-gateway/internal/middleware"
	"pi-gateway/internal/rules"
	"pi-gateway/internal/store"
)

// ErrMigration identifies a legacy configuration which could not be converted.
// The caller may continue using the unchanged legacy chain until it is repaired.
var ErrMigration = errors.New("rules: legacy migration requires repair")

// Service retains only the current immutable compiled snapshot. In-flight requests
// retain their own Engine pointer, so publishing cannot change a running stream.
type Service struct {
	store   *store.Store
	mu      sync.Mutex
	engine  *rules.Engine
	version int64
	ensured bool
}

const languageV2Installed = "language-v2-installed"

func New(st *store.Store) *Service { return &Service{store: st} }

// Ensure initializes legacy rules and applies one-time default-rule upgrades.
func (s *Service) Ensure(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ensureLocked(ctx)
}

func (s *Service) ensureLocked(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.ensured {
		return nil
	}
	version, err := s.store.RuleSetVersion(ctx)
	if err != nil {
		return err
	}
	if version == 0 {
		if _, err := s.store.InitializeRules(ctx, ConvertLegacy, ValidateSnapshot); err != nil {
			return err
		}
	}
	definition := defaultImageExclusion()
	raw, err := EditableJSON(definition)
	if err != nil {
		return err
	}
	if err := s.store.UpgradeDefaultRules(ctx, "internal_default_image_exclusion_v1", []store.RuleChange{
		{Kind: "create", ID: definition.ID, Rule: raw, Source: "default"},
	}, ValidateSnapshot); err != nil {
		return err
	}
	if err := s.upgrade(ctx); err != nil {
		return err
	}
	if err := s.upgradeProtocolProfile(ctx); err != nil {
		return err
	}
	s.ensured = true
	return nil
}

// Load checks the shared version on every new request, but never per response
// frame. A storage error is reported rather than silently using a revoked rule.
func (s *Service) Load(ctx context.Context) (*rules.Engine, int64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ensureLocked(ctx); err != nil {
		return nil, 0, err
	}
	version, err := s.store.RuleSetVersion(ctx)
	if err != nil {
		return nil, 0, err
	}
	if s.engine != nil && s.version == version {
		return s.engine, s.version, nil
	}
	snapshot, err := s.store.LoadRuleSet(ctx)
	if err != nil {
		return nil, 0, err
	}
	if !snapshot.LegacyMigrated {
		return nil, 0, fmt.Errorf("rules: non-migrated published snapshot %d", snapshot.Version)
	}
	definitions, err := DecodeSnapshot(snapshot)
	if err != nil {
		return nil, 0, err
	}
	compiled, err := rules.Compile(definitions)
	if err != nil {
		return nil, 0, err
	}
	s.engine, s.version = compiled, snapshot.Version
	return compiled, snapshot.Version, nil
}

// DecodeSnapshot overlays only trusted persistence metadata. The actual rule
// language is decoded strictly; a malformed saved definition cannot disappear.
func DecodeSnapshot(snapshot *store.RuleSetSnapshot) ([]rules.Rule, error) {
	definitions := make([]rules.Rule, 0, len(snapshot.Rules))
	for _, row := range snapshot.Rules {
		if row == nil {
			return nil, fmt.Errorf("rules: nil persisted row")
		}
		definition, err := rules.ParseRule(row.Rule)
		if err != nil {
			return nil, fmt.Errorf("rule %s: %w", row.ID, err)
		}
		definition.ID = row.ID
		definition.Revision = row.Revision
		definition.OrderIndex = row.OrderIndex
		definition.CreatedAt = formatTimestamp(row.CreatedAt)
		definition.UpdatedAt = formatTimestamp(row.UpdatedAt)
		definition.LegacyName = row.LegacyName
		definition.Source = row.Source
		definitions = append(definitions, definition)
	}
	return definitions, nil
}

// ValidateSnapshot is deliberately pure: publication calls it with its database
// transaction locked, so it must never query the Store or initialize another set.
func ValidateSnapshot(ctx context.Context, snapshot *store.RuleSetSnapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	definitions, err := DecodeSnapshot(snapshot)
	if err != nil {
		return err
	}
	_, err = rules.Compile(definitions)
	return err
}

func formatTimestamp(ms int64) string {
	if ms <= 0 {
		return ""
	}
	return time.UnixMilli(ms).UTC().Format(time.RFC3339Nano)
}

// EditableJSON omits read-only metadata from a rule's persisted/editor AST.
func EditableJSON(definition rules.Rule) (json.RawMessage, error) {
	definition, err := rules.ExpandRule(definition)
	if err != nil {
		return nil, err
	}
	raw, err := json.Marshal(definition)
	if err != nil {
		return nil, err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for _, name := range []string{"id", "revision", "order_index", "created_at", "updated_at", "legacy_name", "source"} {
		delete(fields, name)
	}
	return json.Marshal(fields)
}

// ConvertLegacy preserves effective legacy behavior, including missing registry
// entries and otherwise ignored legacy fields. It is safe inside a transaction.
func ConvertLegacy(ctx context.Context, rows []*store.MiddlewareRow) ([]*store.RuleRow, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	registrations := make([]middleware.Registration, 0, len(rows))
	for _, row := range rows {
		if row == nil {
			continue
		}
		registrations = append(registrations, middleware.Registration{
			Name: row.Name, Enabled: row.Enabled, Order: row.OrderIndex, Config: row.Config,
		})
	}
	definitions, err := rules.MigrateLegacy(registrations)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrMigration, err)
	}
	filter := defaultImageExclusion()
	filter.OrderIndex = int64(len(definitions))
	definitions = append(definitions, filter)
	definitions = append(definitions, rules.DefaultProfile()...)
	definitions = append(definitions, rules.Rule{SchemaVersion: rules.SchemaVersion, ID: languageV2Installed, Name: "Rules V2 installed", Enabled: false, Phase: rules.PhaseRequest, When: rules.Condition{Op: "always"}, Actions: []rules.Action{}, OnError: rules.OnErrorAbort, Source: "system"})
	result := make([]*store.RuleRow, 0, len(definitions))
	for _, definition := range definitions {
		definition, err = rules.ExpandRule(definition)
		if err != nil {
			return nil, err
		}
		raw, err := EditableJSON(definition)
		if err != nil {
			return nil, fmt.Errorf("%w: %w", ErrMigration, err)
		}
		result = append(result, &store.RuleRow{
			ID: definition.ID, Rule: raw, OrderIndex: definition.OrderIndex,
			LegacyName: definition.LegacyName, Source: func() string {
				if definition.Source != "" {
					return definition.Source
				}
				if definition.LegacyName == "" {
					return "default"
				}
				return "legacy"
			}(),
		})
	}
	return result, nil
}

// upgrade publishes expansion and protocol defaults atomically under the existing
// ruleset revision lock. A persisted marker prevents deleted defaults reappearing.
func (s *Service) upgrade(ctx context.Context) error {
	installed, err := s.store.RulesV2Installed(ctx)
	if err != nil {
		return err
	}
	if installed {
		return nil
	}
	for attempt := 0; attempt < 3; attempt++ {
		snapshot, err := s.store.LoadRuleSet(ctx)
		if err != nil {
			return err
		}
		if snapshotHas(snapshot, languageV2Installed) {
			installed, err := s.store.RestoreRulesV2Marker(ctx)
			if err != nil || installed {
				return err
			}
			continue
		}
		changes, err := upgradeChanges(snapshot)
		if err != nil {
			return err
		}
		_, err = s.store.PublishRules(ctx, &snapshot.Version, changes, ValidateSnapshot)
		if errors.Is(err, store.ErrRuleConflict) {
			continue
		}
		return err
	}
	return store.ErrRuleConflict
}
func snapshotHas(s *store.RuleSetSnapshot, id string) bool {
	for _, r := range s.Rules {
		if r.ID == id {
			return true
		}
	}
	return false
}

// Protocol profiles have their own installation marker: an earlier V2 language
// installation may predate these defaults. Existing rules are never overwritten.
func (s *Service) upgradeProtocolProfile(ctx context.Context) error {
	definitions := rules.DefaultProfile()
	changes := make([]store.RuleChange, 0, len(definitions))
	for _, definition := range definitions {
		raw, err := EditableJSON(definition)
		if err != nil {
			return err
		}
		changes = append(changes, store.RuleChange{Kind: "create", ID: definition.ID, Rule: raw, Source: "protocol"})
	}
	return s.store.UpgradeDefaultRules(ctx, "rules.protocol.chatgpt.v1", changes, ValidateSnapshot)
}

func defaultImageExclusion() rules.Rule {
	return rules.Rule{SchemaVersion: rules.SchemaVersion, ID: "default-drop-image-generation", Name: "排除图像生成 / Exclude image generation", Enabled: true, Priority: 1000, Phase: rules.PhaseRequest, When: rules.Condition{Op: "always"}, OnError: rules.OnErrorAbort, Actions: []rules.Action{{ID: "drop-image-generation", Type: "drop_tools", Params: map[string]any{"types": []any{"image_generation"}}}}}
}

func upgradeChanges(snapshot *store.RuleSetSnapshot) ([]store.RuleChange, error) {
	changes, err := expandedRuleChanges(snapshot)
	if err != nil {
		return nil, err
	}
	for _, definition := range rules.DefaultProfile() {
		if snapshotHas(snapshot, definition.ID) {
			continue
		}
		raw, err := EditableJSON(definition)
		if err != nil {
			return nil, err
		}
		changes = append(changes, store.RuleChange{Kind: "create", ID: definition.ID, Rule: raw, Source: "protocol"})
	}
	markerRule := rules.Rule{SchemaVersion: rules.SchemaVersion, ID: languageV2Installed, Name: "规则 V2 已安装 / Rules V2 installed", Enabled: false, Phase: rules.PhaseRequest, When: rules.Condition{Op: "always"}, Actions: []rules.Action{}, OnError: rules.OnErrorAbort}
	raw, err := EditableJSON(markerRule)
	if err != nil {
		return nil, err
	}
	changes = append(changes, store.RuleChange{Kind: "create", ID: markerRule.ID, Rule: raw, Source: "system"})
	return changes, nil
}

func expandedRuleChanges(snapshot *store.RuleSetSnapshot) ([]store.RuleChange, error) {
	changes := []store.RuleChange{}
	for _, row := range snapshot.Rules {
		definition, err := rules.ParseRule(row.Rule)
		if err != nil {
			return nil, err
		}
		expanded, err := rules.ExpandRule(definition)
		if err != nil {
			return nil, err
		}
		raw, err := EditableJSON(expanded)
		if err != nil {
			return nil, err
		}
		if !bytes.Equal(raw, row.Rule) {
			changes = append(changes, store.RuleChange{Kind: "update", ID: row.ID, ExpectedRevision: row.Revision, Rule: raw})
		}
	}
	return changes, nil
}
