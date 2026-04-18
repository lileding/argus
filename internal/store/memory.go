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

// --- QueueStore (in-memory, for CLI mode) ---

func (s *MemoryStore) SaveMessageQueued(_ context.Context, msg *StoredMessage) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	msg.ID = s.nextID
	s.nextID++
	if msg.CreatedAt.IsZero() {
		msg.CreatedAt = time.Now()
	}
	status := "received"
	msg.ReplyStatus = &status
	s.messages = append(s.messages, *msg)
	return nil
}

func (s *MemoryStore) UpdateMessageContent(_ context.Context, msgID int64, content string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.messages {
		if s.messages[i].ID == msgID {
			s.messages[i].Content = content
			return nil
		}
	}
	return nil
}

func (s *MemoryStore) SetReplyStatus(_ context.Context, msgID int64, status string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.messages {
		if s.messages[i].ID == msgID {
			s.messages[i].ReplyStatus = &status
			return nil
		}
	}
	return nil
}

func (s *MemoryStore) AckReply(_ context.Context, msgID int64, replyChannelID string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.messages {
		if s.messages[i].ID == msgID {
			// Only store reply_channel_id, don't change status.
			s.messages[i].ReplyChannelID = replyChannelID
			return nil
		}
	}
	return nil
}

func (s *MemoryStore) ClaimNextReply(_ context.Context, chatID string) (*StoredMessage, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.messages {
		if s.messages[i].ChatID == chatID && s.messages[i].ReplyStatus != nil && *s.messages[i].ReplyStatus == "ready" {
			status := "processing"
			s.messages[i].ReplyStatus = &status
			m := s.messages[i]
			return &m, nil
		}
	}
	return nil, nil
}

func (s *MemoryStore) FinishReply(_ context.Context, msgID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.messages {
		if s.messages[i].ID == msgID {
			status := "done"
			s.messages[i].ReplyStatus = &status
			return nil
		}
	}
	return nil
}

func (s *MemoryStore) RecoverQueue(_ context.Context) (int, []StoredMessage, error) {
	return 0, nil, nil // in-memory: nothing to recover
}

func (s *MemoryStore) PendingChats(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := map[string]bool{}
	var chats []string
	for _, m := range s.messages {
		if m.ReplyStatus != nil && *m.ReplyStatus == "ready" && !seen[m.ChatID] {
			seen[m.ChatID] = true
			chats = append(chats, m.ChatID)
		}
	}
	return chats, nil
}
