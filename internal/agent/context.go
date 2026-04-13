package agent

import (
	"context"

	"argus/internal/model"
	"argus/internal/store"
)

// buildContext loads recent messages and assembles the model input.
func (a *Agent) buildContext(ctx context.Context, chatID, userMessage string) ([]model.Message, error) {
	recent, err := a.store.RecentMessages(ctx, chatID, a.contextWindow)
	if err != nil {
		return nil, err
	}

	messages := make([]model.Message, 0, len(recent)+2)

	// System prompt.
	messages = append(messages, model.Message{
		Role:    model.RoleSystem,
		Content: a.systemPrompt,
	})

	// Recent history.
	for _, m := range recent {
		messages = append(messages, model.Message{
			Role:    model.Role(m.Role),
			Content: m.Content,
		})
	}

	// Current user message.
	messages = append(messages, model.Message{
		Role:    model.RoleUser,
		Content: userMessage,
	})

	return messages, nil
}

// storedToModel converts a stored message into a model message, preserving tool call fields.
func storedToModel(m store.StoredMessage) model.Message {
	msg := model.Message{
		Role:    model.Role(m.Role),
		Content: m.Content,
	}
	if m.ToolCallID != nil {
		msg.ToolCallID = *m.ToolCallID
	}
	return msg
}
