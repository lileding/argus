package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"

	"argus/internal/store"
)

// ForgetTool allows the agent to delete a pinned memory.
type ForgetTool struct {
	store store.PinnedMemoryStore
}

func NewForgetTool(s store.PinnedMemoryStore) *ForgetTool {
	return &ForgetTool{store: s}
}

func (t *ForgetTool) Name() string { return "forget" }

func (t *ForgetTool) Description() string {
	return "Delete a previously saved memory by its ID. Use when the user says to forget something or when a memory is no longer accurate."
}

func (t *ForgetTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Memory ID to delete"}
		},
		"required": ["id"]
	}`)
}

type forgetArgs struct {
	ID string `json:"id"`
}

func (t *ForgetTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args forgetArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	id, err := strconv.ParseInt(args.ID, 10, 64)
	if err != nil {
		return "", fmt.Errorf("invalid memory ID: %s", args.ID)
	}

	if err := t.store.DeleteMemory(ctx, id); err != nil {
		return "", fmt.Errorf("delete memory: %w", err)
	}

	return fmt.Sprintf("Memory %d forgotten.", id), nil
}
