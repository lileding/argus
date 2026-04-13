package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"argus/internal/skill"
)

// ActivateSkillTool lets the LLM load a skill's full prompt mid-conversation.
// Used when the keyword pre-filter didn't match but the LLM recognizes a need
// from the skill catalog.
type ActivateSkillTool struct {
	index *skill.SkillIndex
}

func NewActivateSkillTool(index *skill.SkillIndex) *ActivateSkillTool {
	return &ActivateSkillTool{index: index}
}

func (t *ActivateSkillTool) Name() string { return "activate_skill" }

func (t *ActivateSkillTool) Description() string {
	return "Load the full instructions for a skill by name. Use this when you see a relevant skill in the Available Skills catalog but its detailed instructions aren't already in context."
}

func (t *ActivateSkillTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Skill name from the catalog"}
		},
		"required": ["name"]
	}`)
}

type activateSkillArgs struct {
	Name string `json:"name"`
}

func (t *ActivateSkillTool) Execute(_ context.Context, arguments string) (string, error) {
	var args activateSkillArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	entry, ok := t.index.Get(args.Name)
	if !ok {
		return "", fmt.Errorf("skill %q not found", args.Name)
	}

	return fmt.Sprintf("## Skill: %s\n\n%s", entry.Name, entry.Prompt), nil
}
