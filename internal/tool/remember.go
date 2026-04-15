package tool

import (
	"context"
	"encoding/json"
	"fmt"

	"argus/internal/store"
)

// RememberTool allows the agent to save persistent memories about the user.
type RememberTool struct {
	store store.PinnedMemoryStore
}

func NewRememberTool(s store.PinnedMemoryStore) *RememberTool {
	return &RememberTool{store: s}
}

func (t *RememberTool) Name() string { return "remember" }

func (t *RememberTool) Description() string {
	return "Save an important fact or preference about the user as a persistent memory. " +
		"Use this when the user says 'remember that...', or when you learn something important " +
		"about the user's preferences, identity, or recurring needs. " +
		"Memories persist across all conversations and channels."
}

func (t *RememberTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"content": {
				"type": "string",
				"description": "The fact or preference to remember, as a clear declarative sentence"
			},
			"category": {
				"type": "string",
				"enum": ["preference", "fact", "instruction"],
				"description": "Category: preference (likes/dislikes), fact (biographical info), instruction (behavioral directive)"
			}
		},
		"required": ["content", "category"]
	}`)
}

type rememberArgs struct {
	Content  string `json:"content"`
	Category string `json:"category"`
}

func (t *RememberTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args rememberArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	mem := &store.Memory{
		Content:  args.Content,
		Category: args.Category,
	}
	if err := t.store.SaveMemory(ctx, mem); err != nil {
		return "", fmt.Errorf("save memory: %w", err)
	}

	return fmt.Sprintf("Remembered (id=%d, category=%s): %s", mem.ID, args.Category, args.Content), nil
}
