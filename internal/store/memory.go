package store

import (
	"context"
	"sync"
	"time"
)

// MemoryStore is an in-memory Store implementation for CLI testing.
type MemoryStore struct {
	mu       sync.Mutex
	messages []StoredMessage
	nextID   int64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{nextID: 1}
}

func (s *MemoryStore) SaveMessage(_ context.Context, msg *StoredMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	msg.ID = s.nextID
	s.nextID++
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	s.messages = append(s.messages, *msg)
	return nil
}

func (s *MemoryStore) RecentMessages(_ context.Context, chatID string, limit int) ([]StoredMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var filtered []StoredMessage
	for _, m := range s.messages {
		if m.ChatID == chatID {
			filtered = append(filtered, m)
		}
	}

	// Return the last `limit` messages.
	if len(filtered) > limit {
		filtered = filtered[len(filtered)-limit:]
	}
	return filtered, nil
}
