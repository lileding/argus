package tool

import "context"

type contextKey string

const chatIDKey contextKey = "chat_id"

// WithChatID injects a chat ID into the context for tools to use.
func WithChatID(ctx context.Context, chatID string) context.Context {
	return context.WithValue(ctx, chatIDKey, chatID)
}

// ChatIDFromContext extracts the chat ID from context.
func ChatIDFromContext(ctx context.Context) string {
	v, _ := ctx.Value(chatIDKey).(string)
	return v
}
