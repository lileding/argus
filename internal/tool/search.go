package tool

import (
	"context"
	"encoding/json"
	"fmt"
)

// SearchTool is a web search tool (stub for MVP).
type SearchTool struct{}

func NewSearchTool() *SearchTool {
	return &SearchTool{}
}

func (t *SearchTool) Name() string { return "search" }

func (t *SearchTool) Description() string {
	return "Search the web for real-time information, news, or knowledge. Returns search results."
}

func (t *SearchTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "Search query"}
		},
		"required": ["query"]
	}`)
}

type searchArgs struct {
	Query string `json:"query"`
}

func (t *SearchTool) Execute(_ context.Context, arguments string) (string, error) {
	var args searchArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}

	return fmt.Sprintf("搜索功能尚未实现。查询: %s", args.Query), nil
}
