// Package compactprompt contains the default prompt for model summary compaction.
package compactprompt

import _ "embed" // Required by go:embed for the bundled compaction prompt.

// Default is copied verbatim from OpenAI Codex (Apache-2.0):
// https://github.com/openai/codex/blob/a6baf8867cb4c9726213c0884a5c8b11f0cfd8bf/codex-rs/prompts/templates/compact/prompt.md
// Copyright OpenAI. See LICENSE.txt in this directory.
//
//go:embed prompt.md
var Default string

// MaxBytes bounds custom prompts accepted by the settings API.
const MaxBytes = 64 << 10
