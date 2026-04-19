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
	tasks    []Task
	nextTask int64
	outbox   []OutboxEvent
	nextOut  int64
	cron     []CronSchedule
	nextCron int64
}

func NewMemoryStore() *MemoryStore {
	return &MemoryStore{nextID: 1, nextTask: 1, nextOut: 1, nextCron: 1}
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

func (s *MemoryStore) UpdateMessageFilePaths(_ context.Context, msgID int64, paths []string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.messages {
		if s.messages[i].ID == msgID {
			s.messages[i].FilePaths = paths
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

// --- TaskStore (in-memory, for tests / no-DB mode) ---

func (s *MemoryStore) CreateTask(_ context.Context, task *Task) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	task.ID = s.nextTask
	s.nextTask++
	if task.Kind == "" {
		task.Kind = "async"
	}
	if task.Source == "" {
		task.Source = "agent"
	}
	if task.Status == "" {
		task.Status = "queued"
	}
	if len(task.Input) == 0 {
		task.Input = []byte(`{}`)
	}
	if task.CreatedAt.IsZero() {
		task.CreatedAt = time.Now()
	}
	s.tasks = append(s.tasks, *task)
	return nil
}

func (s *MemoryStore) GetTask(_ context.Context, taskID int64) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, t := range s.tasks {
		if t.ID == taskID {
			task := t
			return &task, nil
		}
	}
	return nil, nil
}

func (s *MemoryStore) ClaimNextTask(_ context.Context, workerID string, leaseUntil time.Time) (*Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bestIdx := -1
	for i := range s.tasks {
		if s.tasks[i].Status != "queued" {
			continue
		}
		if bestIdx == -1 ||
			s.tasks[i].Priority > s.tasks[bestIdx].Priority ||
			(s.tasks[i].Priority == s.tasks[bestIdx].Priority && s.tasks[i].CreatedAt.Before(s.tasks[bestIdx].CreatedAt)) {
			bestIdx = i
		}
	}
	if bestIdx == -1 {
		return nil, nil
	}

	now := time.Now()
	s.tasks[bestIdx].Status = "running"
	s.tasks[bestIdx].LeaseOwner = workerID
	s.tasks[bestIdx].LeaseUntil = &leaseUntil
	if s.tasks[bestIdx].StartedAt == nil {
		s.tasks[bestIdx].StartedAt = &now
	}
	task := s.tasks[bestIdx]
	return &task, nil
}

func (s *MemoryStore) CompleteTask(_ context.Context, taskID int64, result string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for i := range s.tasks {
		if s.tasks[i].ID == taskID && s.tasks[i].Status == "running" {
			s.tasks[i].Status = "succeeded"
			s.tasks[i].Result = result
			s.tasks[i].Error = ""
			s.tasks[i].LeaseOwner = ""
			s.tasks[i].LeaseUntil = nil
			s.tasks[i].FinishedAt = &now
			return nil
		}
	}
	return nil
}

func (s *MemoryStore) FailTask(_ context.Context, taskID int64, errorMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for i := range s.tasks {
		if s.tasks[i].ID == taskID && s.tasks[i].Status == "running" {
			s.tasks[i].Status = "failed"
			s.tasks[i].Error = errorMsg
			s.tasks[i].LeaseOwner = ""
			s.tasks[i].LeaseUntil = nil
			s.tasks[i].FinishedAt = &now
			return nil
		}
	}
	return nil
}

func (s *MemoryStore) CancelTask(_ context.Context, taskID int64) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for i := range s.tasks {
		if s.tasks[i].ID != taskID {
			continue
		}
		if s.tasks[i].Status != "queued" && s.tasks[i].Status != "running" {
			return false, nil
		}
		s.tasks[i].Status = "cancelled"
		s.tasks[i].LeaseOwner = ""
		s.tasks[i].LeaseUntil = nil
		s.tasks[i].FinishedAt = &now
		return true, nil
	}
	return false, nil
}

func (s *MemoryStore) RecoverExpiredTasks(_ context.Context, now time.Time) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	recovered := 0
	for i := range s.tasks {
		if s.tasks[i].Status == "running" && s.tasks[i].LeaseUntil != nil && s.tasks[i].LeaseUntil.Before(now) {
			s.tasks[i].Status = "queued"
			s.tasks[i].LeaseOwner = ""
			s.tasks[i].LeaseUntil = nil
			recovered++
		}
	}
	return recovered, nil
}

// --- OutboxStore (in-memory, for tests / no-DB mode) ---

func (s *MemoryStore) CreateOutboxEvent(_ context.Context, event *OutboxEvent) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	event.ID = s.nextOut
	s.nextOut++
	if event.Kind == "" {
		event.Kind = "notice"
	}
	if event.Status == "" {
		event.Status = "pending"
	}
	if len(event.Payload) == 0 {
		event.Payload = []byte(`{}`)
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now()
	}
	s.outbox = append(s.outbox, *event)
	return nil
}

func (s *MemoryStore) PendingOutboxChats(_ context.Context) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	seen := map[string]bool{}
	var chats []string
	for _, e := range s.outbox {
		if e.Status == "pending" && !seen[e.ChatID] {
			seen[e.ChatID] = true
			chats = append(chats, e.ChatID)
		}
	}
	return chats, nil
}

func (s *MemoryStore) ClaimNextOutboxEvent(_ context.Context, chatID string) (*OutboxEvent, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	bestIdx := -1
	for i := range s.outbox {
		if s.outbox[i].ChatID != chatID || s.outbox[i].Status != "pending" {
			continue
		}
		if bestIdx == -1 ||
			s.outbox[i].Priority > s.outbox[bestIdx].Priority ||
			(s.outbox[i].Priority == s.outbox[bestIdx].Priority && s.outbox[i].CreatedAt.Before(s.outbox[bestIdx].CreatedAt)) {
			bestIdx = i
		}
	}
	if bestIdx == -1 {
		return nil, nil
	}
	s.outbox[bestIdx].Status = "sending"
	event := s.outbox[bestIdx]
	return &event, nil
}

func (s *MemoryStore) MarkOutboxSent(_ context.Context, eventID int64) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for i := range s.outbox {
		if s.outbox[i].ID == eventID {
			s.outbox[i].Status = "sent"
			s.outbox[i].Error = ""
			s.outbox[i].SentAt = &now
			return nil
		}
	}
	return nil
}

func (s *MemoryStore) MarkOutboxError(_ context.Context, eventID int64, errorMsg string) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for i := range s.outbox {
		if s.outbox[i].ID == eventID {
			s.outbox[i].Status = "failed"
			s.outbox[i].Error = errorMsg
			return nil
		}
	}
	return nil
}

func (s *MemoryStore) RecoverOutbox(_ context.Context) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	recovered := 0
	for i := range s.outbox {
		if s.outbox[i].Status == "sending" {
			s.outbox[i].Status = "pending"
			recovered++
		}
	}
	return recovered, nil
}

// --- CronStore (in-memory, for tests / no-DB mode) ---

func (s *MemoryStore) CreateCronSchedule(_ context.Context, schedule *CronSchedule) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	schedule.ID = s.nextCron
	s.nextCron++
	if schedule.ScheduleType == "" {
		schedule.ScheduleType = "daily"
	}
	if schedule.Timezone == "" {
		schedule.Timezone = "Asia/Shanghai"
	}
	schedule.Enabled = true
	now := time.Now()
	if schedule.CreatedAt.IsZero() {
		schedule.CreatedAt = now
	}
	if schedule.UpdatedAt.IsZero() {
		schedule.UpdatedAt = now
	}
	s.cron = append(s.cron, *schedule)
	return nil
}

func (s *MemoryStore) ListCronSchedules(_ context.Context, chatID string, includeDisabled bool) ([]CronSchedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var out []CronSchedule
	for _, sched := range s.cron {
		if sched.ChatID != chatID {
			continue
		}
		if !includeDisabled && !sched.Enabled {
			continue
		}
		out = append(out, sched)
	}
	return out, nil
}

func (s *MemoryStore) DeleteCronSchedule(_ context.Context, scheduleID int64, chatID string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for i := range s.cron {
		if s.cron[i].ID == scheduleID && s.cron[i].ChatID == chatID && s.cron[i].Enabled {
			s.cron[i].Enabled = false
			s.cron[i].UpdatedAt = now
			return true, nil
		}
	}
	return false, nil
}

func (s *MemoryStore) DueCronSchedules(_ context.Context, now time.Time, limit int) ([]CronSchedule, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if limit <= 0 {
		limit = 20
	}
	var out []CronSchedule
	for _, sched := range s.cron {
		if !sched.Enabled || sched.NextRunAt == nil || sched.NextRunAt.After(now) {
			continue
		}
		out = append(out, sched)
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

func (s *MemoryStore) MarkCronScheduleRun(_ context.Context, scheduleID int64, lastRunAt, nextRunAt time.Time) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	now := time.Now()
	for i := range s.cron {
		if s.cron[i].ID == scheduleID {
			s.cron[i].LastRunAt = &lastRunAt
			s.cron[i].NextRunAt = &nextRunAt
			s.cron[i].UpdatedAt = now
			return nil
		}
	}
	return nil
}
