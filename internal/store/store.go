package store

import (
	"context"
	"time"
)

// StoredMessage represents a message persisted in the store.
type StoredMessage struct {
	ID         int64
	ChatID     string
	Role       string
	Content    string
	ToolName   *string
	ToolCallID *string
	CreatedAt  time.Time
}

// Store is the interface for message persistence.
type Store interface {
	SaveMessage(ctx context.Context, msg *StoredMessage) error
	RecentMessages(ctx context.Context, chatID string, limit int) ([]StoredMessage, error)
}
