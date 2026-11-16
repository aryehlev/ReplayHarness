// Package replayharness holds the agent files embedded into the CLI.
package replayharness

import (
	_ "embed"
	"strings"
)

var (
	//go:embed skills/replayharness/SKILL.md
	skill string
	//go:embed agents/replayharness-discovery.md
	discoveryAgent string
	//go:embed agents/replayharness-replay.md
	replayAgent string
)

// AgentFiles maps where each file goes in a project (or in ~/) to its content:
// the Claude Code skill that runs the two steps, and the two agents.
func AgentFiles() map[string]string {
	return map[string]string{
		".claude/skills/replayharness/SKILL.md":     skill,
		".claude/agents/replayharness-discovery.md": discoveryAgent,
		".claude/agents/replayharness-replay.md":    replayAgent,
	}
}

// Playbook is all three files as one document, for agents without skills or
// subagents (paste it into AGENTS.md or the agent's instructions).
func Playbook() string {
	var b strings.Builder
	b.WriteString(body(skill))
	b.WriteString("\n## Step 1: discovery\n\n")
	b.WriteString(demote(body(discoveryAgent)))
	b.WriteString("\n## Step 2: replays\n\n")
	b.WriteString(demote(body(replayAgent)))
	return b.String()
}

// body drops the YAML front matter.
func body(doc string) string {
	if strings.HasPrefix(doc, "---\n") {
		if end := strings.Index(doc[4:], "\n---\n"); end >= 0 {
			doc = doc[4+end+5:]
		}
	}
	return strings.TrimLeft(doc, "\n")
}

// demote turns "## x" into "### x" so the parts nest under the step headings.
func demote(doc string) string {
	lines := strings.Split(doc, "\n")
	inCode := false
	for i, l := range lines {
		if strings.HasPrefix(l, "```") {
			inCode = !inCode
		}
		if !inCode && strings.HasPrefix(l, "#") {
			lines[i] = "#" + l
		}
	}
	return strings.Join(lines, "\n")
}
