package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"argus/internal/cron"
	"argus/internal/store"
)

type CreateCronTool struct {
	store store.CronStore
}

func NewCreateCronTool(s store.CronStore) *CreateCronTool {
	return &CreateCronTool{store: s}
}

func (t *CreateCronTool) Name() string { return "create_cron" }

func (t *CreateCronTool) Description() string {
	return "Create a persistent daily schedule that will run an async task at a specific local time. " +
		"Use when the user asks for recurring work such as daily summaries or reminders. " +
		"Initial support is daily schedules only."
}

func (t *CreateCronTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"name": {"type": "string", "description": "Short user-visible schedule name"},
			"hour": {"type": "integer", "description": "Hour of day, 0-23, in the given timezone"},
			"minute": {"type": "integer", "description": "Minute of hour, 0-59"},
			"timezone": {"type": "string", "description": "IANA timezone, default Asia/Shanghai"},
			"prompt": {"type": "string", "description": "Complete prompt to run each time the schedule fires"}
		},
		"required": ["name", "hour", "minute", "prompt"]
	}`)
}

type createCronArgs struct {
	Name     string `json:"name"`
	Hour     int    `json:"hour"`
	Minute   int    `json:"minute"`
	Timezone string `json:"timezone"`
	Prompt   string `json:"prompt"`
}

func (t *CreateCronTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args createCronArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	args.Name = strings.TrimSpace(args.Name)
	args.Prompt = strings.TrimSpace(args.Prompt)
	args.Timezone = strings.TrimSpace(args.Timezone)
	if args.Timezone == "" {
		args.Timezone = "Asia/Shanghai"
	}
	if args.Name == "" {
		return "", fmt.Errorf("name is required")
	}
	if args.Prompt == "" {
		return "", fmt.Errorf("prompt is required")
	}

	next, err := cron.NextDailyRun(time.Now(), args.Hour, args.Minute, args.Timezone)
	if err != nil {
		return "", err
	}
	schedule := &store.CronSchedule{
		ChatID:       ChatIDFromContext(ctx),
		Name:         args.Name,
		ScheduleType: "daily",
		Hour:         args.Hour,
		Minute:       args.Minute,
		Timezone:     args.Timezone,
		Prompt:       args.Prompt,
		NextRunAt:    &next,
	}
	if schedule.ChatID == "" {
		schedule.ChatID = "unknown"
	}
	if err := t.store.CreateCronSchedule(ctx, schedule); err != nil {
		return "", fmt.Errorf("create cron schedule: %w", err)
	}

	return fmt.Sprintf("Created daily schedule %d: %s at %02d:%02d %s. Next run: %s.",
		schedule.ID, schedule.Name, schedule.Hour, schedule.Minute, schedule.Timezone,
		next.Format(time.RFC3339)), nil
}

type ListCronTool struct {
	store store.CronStore
}

func NewListCronTool(s store.CronStore) *ListCronTool {
	return &ListCronTool{store: s}
}

func (t *ListCronTool) Name() string { return "list_cron" }

func (t *ListCronTool) Description() string {
	return "List persistent schedules for the current chat."
}

func (t *ListCronTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"include_disabled": {"type": "boolean", "description": "Include disabled schedules"}
		}
	}`)
}

type listCronArgs struct {
	IncludeDisabled bool `json:"include_disabled"`
}

func (t *ListCronTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args listCronArgs
	if strings.TrimSpace(arguments) != "" {
		if err := json.Unmarshal([]byte(arguments), &args); err != nil {
			return "", fmt.Errorf("parse arguments: %w", err)
		}
	}
	chatID := ChatIDFromContext(ctx)
	schedules, err := t.store.ListCronSchedules(ctx, chatID, args.IncludeDisabled)
	if err != nil {
		return "", fmt.Errorf("list cron schedules: %w", err)
	}
	if len(schedules) == 0 {
		return "No schedules for this chat.", nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Schedules (%d):\n", len(schedules)))
	for _, s := range schedules {
		status := "enabled"
		if !s.Enabled {
			status = "disabled"
		}
		next := ""
		if s.NextRunAt != nil {
			next = s.NextRunAt.Format(time.RFC3339)
		}
		sb.WriteString(fmt.Sprintf("- id=%d %s daily %02d:%02d %s (%s, next=%s)\n",
			s.ID, s.Name, s.Hour, s.Minute, s.Timezone, status, next))
	}
	return strings.TrimSpace(sb.String()), nil
}

type DeleteCronTool struct {
	store store.CronStore
}

func NewDeleteCronTool(s store.CronStore) *DeleteCronTool {
	return &DeleteCronTool{store: s}
}

func (t *DeleteCronTool) Name() string { return "delete_cron" }

func (t *DeleteCronTool) Description() string {
	return "Disable a persistent schedule by ID for the current chat."
}

func (t *DeleteCronTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"id": {"type": "string", "description": "Schedule ID"}
		},
		"required": ["id"]
	}`)
}

type deleteCronArgs struct {
	ID string `json:"id"`
}

func (t *DeleteCronTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args deleteCronArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	id, err := strconv.ParseInt(args.ID, 10, 64)
	if err != nil || id <= 0 {
		return "", fmt.Errorf("invalid schedule ID: %s", args.ID)
	}
	deleted, err := t.store.DeleteCronSchedule(ctx, id, ChatIDFromContext(ctx))
	if err != nil {
		return "", fmt.Errorf("delete cron schedule: %w", err)
	}
	if !deleted {
		return fmt.Sprintf("Schedule %d was not disabled. It may not exist, belong to another chat, or already be disabled.", id), nil
	}
	return fmt.Sprintf("Schedule %d disabled.", id), nil
}
