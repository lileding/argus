package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"argus/internal/store"
)

// CreateAsyncTaskTool lets the orchestrator create durable background work.
type CreateAsyncTaskTool struct {
	store store.TaskStore
}

func NewCreateAsyncTaskTool(s store.TaskStore) *CreateAsyncTaskTool {
	return &CreateAsyncTaskTool{store: s}
}

func (t *CreateAsyncTaskTool) Name() string { return "create_async_task" }

func (t *CreateAsyncTaskTool) Description() string {
	return "Create a durable background task for long-running work. " +
		"Use only for code work, large document processing, multi-step research, explicit background requests, " +
		"or work that should finish later and notify the user. Do not use for short questions, simple searches, " +
		"or small database queries."
}

func (t *CreateAsyncTaskTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"title": {"type": "string", "description": "Short user-visible task title"},
			"prompt": {"type": "string", "description": "Complete instructions for the background task"},
			"priority": {"type": "integer", "description": "Optional priority; higher runs first, default 0"}
		},
		"required": ["title", "prompt"]
	}`)
}

type createAsyncTaskArgs struct {
	Title    string `json:"title"`
	Prompt   string `json:"prompt"`
	Priority int    `json:"priority"`
}

func (t *CreateAsyncTaskTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args createAsyncTaskArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	args.Title = strings.TrimSpace(args.Title)
	args.Prompt = strings.TrimSpace(args.Prompt)
	if args.Title == "" {
		return "", fmt.Errorf("title is required")
	}
	if args.Prompt == "" {
		return "", fmt.Errorf("prompt is required")
	}

	input, err := json.Marshal(map[string]string{"prompt": args.Prompt})
	if err != nil {
		return "", fmt.Errorf("marshal task input: %w", err)
	}

	task := &store.Task{
		Kind:     "async",
		Source:   "agent",
		ChatID:   ChatIDFromContext(ctx),
		Status:   "queued",
		Priority: args.Priority,
		Title:    args.Title,
		Input:    input,
	}
	if task.ChatID == "" {
		task.ChatID = "unknown"
	}
	if err := t.store.CreateTask(ctx, task); err != nil {
		return "", fmt.Errorf("create task: %w", err)
	}

	return fmt.Sprintf("Created async task %d: %s (status=queued). Use get_task_status to check progress.", task.ID, task.Title), nil
}

// GetTaskStatusTool returns the current state of a durable task.
type GetTaskStatusTool struct {
	store store.TaskStore
}

func NewGetTaskStatusTool(s store.TaskStore) *GetTaskStatusTool {
	return &GetTaskStatusTool{store: s}
}

func (t *GetTaskStatusTool) Name() string { return "get_task_status" }

func (t *GetTaskStatusTool) Description() string {
	return "Get the status, result, or error for a previously created async task."
}

func (t *GetTaskStatusTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Task ID"}
		},
		"required": ["id"]
	}`)
}

type taskIDArgs struct {
	ID string `json:"id"`
}

func (t *GetTaskStatusTool) Execute(ctx context.Context, arguments string) (string, error) {
	id, err := parseTaskID(arguments)
	if err != nil {
		return "", err
	}
	task, err := t.store.GetTask(ctx, id)
	if err != nil {
		return "", fmt.Errorf("get task: %w", err)
	}
	if task == nil {
		return fmt.Sprintf("Task %d not found.", id), nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Task %d: %s\n", task.ID, task.Title))
	sb.WriteString(fmt.Sprintf("Status: %s\n", task.Status))
	if task.Error != "" {
		sb.WriteString(fmt.Sprintf("Error: %s\n", task.Error))
	}
	if task.Result != "" {
		sb.WriteString("Result:\n")
		sb.WriteString(task.Result)
	}
	return strings.TrimSpace(sb.String()), nil
}

// CancelTaskTool cancels a queued or running task.
type CancelTaskTool struct {
	store store.TaskStore
}

func NewCancelTaskTool(s store.TaskStore) *CancelTaskTool {
	return &CancelTaskTool{store: s}
}

func (t *CancelTaskTool) Name() string { return "cancel_task" }

func (t *CancelTaskTool) Description() string {
	return "Cancel a queued or running async task. Completed, failed, and already-cancelled tasks cannot be changed."
}

func (t *CancelTaskTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Task ID"}
		},
		"required": ["id"]
	}`)
}

func (t *CancelTaskTool) Execute(ctx context.Context, arguments string) (string, error) {
	id, err := parseTaskID(arguments)
	if err != nil {
		return "", err
	}
	cancelled, err := t.store.CancelTask(ctx, id)
	if err != nil {
		return "", fmt.Errorf("cancel task: %w", err)
	}
	if !cancelled {
		return fmt.Sprintf("Task %d was not cancelled. It may not exist or may already be in a terminal state.", id), nil
	}
	return fmt.Sprintf("Task %d cancelled.", id), nil
}

func parseTaskID(arguments string) (int64, error) {
	var args taskIDArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return 0, fmt.Errorf("parse arguments: %w", err)
	}
	id, err := strconv.ParseInt(args.ID, 10, 64)
	if err != nil || id <= 0 {
		return 0, fmt.Errorf("invalid task ID: %s", args.ID)
	}
	return id, nil
}
