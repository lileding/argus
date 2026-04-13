package agent

import (
	"context"
	"fmt"
	"log/slog"

	"argus/internal/model"
	"argus/internal/store"
)

// Agent is the core agent loop that processes messages using an LLM.
type Agent struct {
	model         model.Client
	store         store.Store
	systemPrompt  string
	contextWindow int
}

func New(modelClient model.Client, st store.Store, systemPrompt string, contextWindow int) *Agent {
	return &Agent{
		model:         modelClient,
		store:         st,
		systemPrompt:  systemPrompt,
		contextWindow: contextWindow,
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

	slog.Info("calling model", "chat_id", chatID, "messages", len(messages))

	// Call model (no tools in Phase 2).
	resp, err := a.model.Chat(ctx, messages, nil)
	if err != nil {
		return "", fmt.Errorf("model chat: %w", err)
	}

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
