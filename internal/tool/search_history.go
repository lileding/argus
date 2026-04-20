package tool

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"argus/internal/embedding"
	"argus/internal/store"
)

// SearchHistoryTool lets the orchestrator semantically search conversation history.
// This complements the passive recall in curateHistory — the model can actively
// retrieve full message text when the summary in context isn't sufficient.
type SearchHistoryTool struct {
	semantic store.SemanticStore
	embedder *embedding.Client
}

func NewSearchHistoryTool(ss store.SemanticStore, emb *embedding.Client) *SearchHistoryTool {
	return &SearchHistoryTool{semantic: ss, embedder: emb}
}

func (t *SearchHistoryTool) Name() string { return "search_history" }

func (t *SearchHistoryTool) Description() string {
	return "Search past conversation messages by semantic similarity. " +
		"Use this when you see a summary of a previous reply and need the full original text, " +
		"or when the user references something discussed earlier."
}

func (t *SearchHistoryTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"query": {"type": "string", "description": "What to search for in conversation history"},
			"limit": {"type": "integer", "description": "Max results (default 5, max 10)"}
		},
		"required": ["query"]
	}`)
}

type searchHistoryArgs struct {
	Query string `json:"query"`
	Limit int    `json:"limit"`
}

func (t *SearchHistoryTool) Execute(ctx context.Context, arguments string) (string, error) {
	var args searchHistoryArgs
	if err := json.Unmarshal([]byte(arguments), &args); err != nil {
		return "", fmt.Errorf("parse arguments: %w", err)
	}
	if args.Query == "" {
		return "", fmt.Errorf("query is required")
	}
	limit := args.Limit
	if limit <= 0 {
		limit = 5
	}
	if limit > 10 {
		limit = 10
	}

	chatID := ChatIDFromContext(ctx)
	if chatID == "" {
		return "No conversation context available.", nil
	}

	vec, err := t.embedder.EmbedOne(ctx, args.Query)
	if err != nil {
		return "", fmt.Errorf("embed query: %w", err)
	}

	results, err := t.semantic.SearchMessages(ctx, vec, chatID, limit)
	if err != nil {
		return "", fmt.Errorf("search messages: %w", err)
	}

	if len(results) == 0 {
		return "No matching messages found in conversation history.", nil
	}

	var sb strings.Builder
	sb.WriteString(fmt.Sprintf("Found %d messages:\n", len(results)))
	for i, m := range results {
		content := m.Content
		if len(content) > 3000 {
			content = content[:3000] + " …[truncated]"
		}
		sb.WriteString(fmt.Sprintf("\n--- #%d [%s, %s] ---\n%s\n",
			i+1, m.Role, m.CreatedAt.Format("2006-01-02 15:04"), content))
	}
	return sb.String(), nil
}
