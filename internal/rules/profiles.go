package rules

import (
	_ "embed" // Registers go:embed support for the built-in protocol profile.
	"encoding/json"
)

//go:embed profiles/chatgpt-v1.json
var defaultProfileJSON []byte

// DefaultProfile returns detached, editable protocol rules. Protocol field names
// and client fingerprints live in versioned JSON, outside the interpreter.
func DefaultProfile() []Rule {
	var profile struct {
		ProfileVersion int    `json:"profile_version"`
		SchemaVersion  int    `json:"schema_version"`
		Rules          []Rule `json:"rules"`
	}
	if err := json.Unmarshal(defaultProfileJSON, &profile); err != nil {
		panic("invalid embedded protocol profile: " + err.Error())
	}
	return profile.Rules
}
