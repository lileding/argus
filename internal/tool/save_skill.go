package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// SaveSkillTool allows the agent to create or update a SKILL.md file.
type SaveSkillTool struct {
	skillsDir string
	onSave    func() // callback to trigger index rebuild
}

func NewSaveSkillTool(skillsDir string, onSave func()) *SaveSkillTool {
	return &SaveSkillTool{skillsDir: skillsDir, onSave: onSave}
}

func (t *SaveSkillTool) Name() string { return "save_skill" }

func (t *SaveSkillTool) Description() string {
	return "Create or update a skill by writing a SKILL.md file. Use this after successfully completing a new type of task that might recur. The skill captures your approach as reusable instructions."
}

func (t *SaveSkillTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Skill identifier (lowercase letters, numbers, hyphens)"},
			"description": {"type": "string", "description": "What this skill does and when to use it (max 250 chars recommended)"},
			"tools": {"type": "array", "items": {"type": "string"}, "description": "Tool names this skill needs (e.g. [\"db\", \"db_exec\"])"},
			"prompt": {"type": "string", "description": "The full skill instructions in Markdown"},
			"setup_sql": {"type": "string", "description": "Optional: SQL to include as setup.sql (e.g. CREATE TABLE statements)"}
		},
		"required": ["name", "description", "prompt"]
	}`)
}

var validName = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)

type saveSkillArgs struct {
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tools       []string `json:"tools"`
	Prompt      string   `json:"prompt"`
	SetupSQL    string   `json:"setup_sql"`
}

func (t *SaveSkillTool) Execute(_ context.Context, arguments string) (string, error) {
	var args saveSkillArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	if !validName.MatchString(args.Name) {
		return "", fmt.Errorf("invalid skill name %q: must be lowercase letters, numbers, and hyphens", args.Name)
	}

	// Build SKILL.md content.
	var sb strings.Builder
	sb.WriteString("---\n")
	sb.WriteString(fmt.Sprintf("name: %s\n", args.Name))
	sb.WriteString(fmt.Sprintf("description: %q\n", args.Description))
	if len(args.Tools) > 0 {
		sb.WriteString("tools:\n")
		for _, tool := range args.Tools {
			sb.WriteString(fmt.Sprintf("  - %s\n", tool))
		}
	}
	sb.WriteString("---\n\n")
	sb.WriteString(args.Prompt)
	sb.WriteString("\n")

	// Create skill directory.
	skillDir := filepath.Join(t.skillsDir, args.Name)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return "", fmt.Errorf("create skill directory: %w", err)
	}

	// Write SKILL.md.
	skillPath := filepath.Join(skillDir, "SKILL.md")
	if err := os.WriteFile(skillPath, []byte(sb.String()), 0644); err != nil {
		return "", fmt.Errorf("write SKILL.md: %w", err)
	}

	// Write setup.sql if provided.
	if args.SetupSQL != "" {
		sqlPath := filepath.Join(skillDir, "setup.sql")
		if err := os.WriteFile(sqlPath, []byte(args.SetupSQL), 0644); err != nil {
			return "", fmt.Errorf("write setup.sql: %w", err)
		}
	}

	// Trigger index rebuild.
	if t.onSave != nil {
		t.onSave()
	}

	return fmt.Sprintf("Skill '%s' saved to %s", args.Name, skillPath), nil
}
