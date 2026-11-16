// Package replayharness holds files embedded into the CLI.
package replayharness

import _ "embed"

// Skill is the agent playbook: a Claude Code skill, also usable as plain
// instructions (AGENTS.md) for other coding agents.
//
//go:embed skills/replayharness/SKILL.md
var Skill string
