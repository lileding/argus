package agent

import (
	"context"
	"fmt"
	"log/slog"

	"argus/internal/model"
	"argus/internal/store"
	"argus/internal/tool"
)

// Agent is the core agent loop that processes messages using an LLM with tools.
type Agent struct {
	model         model.Client
	store         store.Store
	registry      *tool.Registry
	systemPrompt  string
	contextWindow int
	maxIterations int
}

func New(modelClient model.Client, st store.Store, registry *tool.Registry, systemPrompt string, contextWindow, maxIterations int) *Agent {
	if maxIterations == 0 {
		maxIterations = 10
	}
	return &Agent{
		model:         modelClient,
		store:         st,
		registry:      registry,
		systemPrompt:  systemPrompt,
		contextWindow: contextWindow,
		maxIterations: maxIterations,
	}
}

// Handle processes a user message and returns the assistant's reply.
func (a *Agent) Handle(ctx context.Context, chatID, userMessage string) (string, error) {
	// Save user message.
	if err := a.store.SaveMessage(ctx, &store.StoredMessage{
		ChatID:  chatID,
		Role:    string(model.RoleUser),
		Content: userMessage,
	}); err != nil {
		return "", fmt.Errorf("save user message: %w", err)
	}

	// Build context with recent history.
	messages, err := a.buildContext(ctx, chatID, userMessage)
	if err != nil {
		return "", fmt.Errorf("build context: %w", err)
	}

	// Get tool definitions.
	var toolDefs []model.ToolDef
	if a.registry != nil && a.registry.Len() > 0 {
		toolDefs = a.registry.AllToolDefs()
	}

	// Agent tool loop.
	for i := 0; i < a.maxIterations; i++ {
		slog.Info("calling model", "chat_id", chatID, "iteration", i, "messages", len(messages))

		resp, err := a.model.Chat(ctx, messages, toolDefs)
		if err != nil {
			return "", fmt.Errorf("model chat (iteration %d): %w", i, err)
		}

		// If no tool calls, we're done.
		if len(resp.ToolCalls) == 0 || resp.FinishReason == "stop" {
			reply := resp.Content

			// Save assistant reply.
			if err := a.store.SaveMessage(ctx, &store.StoredMessage{
				ChatID:  chatID,
				Role:    string(model.RoleAssistant),
				Content: reply,
			}); err != nil {
				return "", fmt.Errorf("save assistant reply: %w", err)
			}

			return reply, nil
		}

		// Append assistant message with tool calls.
		assistantMsg := model.Message{
			Role:      model.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		}
		messages = append(messages, assistantMsg)

		// Execute each tool call and append results.
		for _, tc := range resp.ToolCalls {
			result := a.executeTool(ctx, tc)

			slog.Info("tool executed",
				"tool", tc.Function.Name,
				"call_id", tc.ID,
				"result_len", len(result),
			)

			messages = append(messages, model.Message{
				Role:       model.RoleTool,
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}

	return "", fmt.Errorf("agent loop exceeded max iterations (%d)", a.maxIterations)
}

// executeTool runs a tool and returns the result as a string.
// Errors are returned as the result string so the model can see them.
func (a *Agent) executeTool(ctx context.Context, tc model.ToolCall) string {
	t, ok := a.registry.Get(tc.Function.Name)
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", tc.Function.Name)
	}

	result, err := t.Execute(ctx, tc.Function.Arguments)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	return result
}
