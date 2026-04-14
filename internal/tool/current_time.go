package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"time"
)

type CurrentTimeTool struct{}

func NewCurrentTimeTool() *CurrentTimeTool { return &CurrentTimeTool{} }

func (t *CurrentTimeTool) Name() string { return "current_time" }

func (t *CurrentTimeTool) Description() string {
	return "Get the current date and time. Use this when you need to know today's date, current time, day of week, or resolve relative time references like 'today', 'tomorrow', 'last week'."
}

func (t *CurrentTimeTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"timezone": {"type": "string", "description": "IANA timezone (e.g. America/New_York). Default: system local timezone."}
		}
	}`)
}

type currentTimeArgs struct {
	Timezone string `json:"timezone"`
}

func (t *CurrentTimeTool) Execute(_ context.Context, arguments string) (string, error) {
	var args currentTimeArgs
	json.Unmarshal([]byte(arguments), &args)

	loc := time.Now().Location()
	if args.Timezone != "" {
		parsed, err := time.LoadLocation(args.Timezone)
		if err != nil {
			return "", fmt.Errorf("invalid timezone %q: %w", args.Timezone, err)
		}
		loc = parsed
	}

	now := time.Now().In(loc)
	zone, offset := now.Zone()
	offsetHours := offset / 3600

	return fmt.Sprintf(
		"ISO 8601: %s\nDate: %s\nTime: %s\nDay: %s\nTimezone: %s (UTC%+d)\nUnix: %d\nWeek: %d, Day of year: %d",
		now.Format(time.RFC3339),
		now.Format("2006-01-02"),
		now.Format("15:04:05"),
		now.Format("Monday"),
		zone, offsetHours,
		now.Unix(),
		now.YearDay()/7+1, now.YearDay(),
	), nil
}
