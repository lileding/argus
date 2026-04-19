package store

import (
	"context"
	"time"
)

// StoredMessage represents a message persisted in the store.
type StoredMessage struct {
	ID         int64
	ChatID     string // composite key: "feishu:p2p:ou_xxx", "cli:local"
	Role       string // user / assistant / tool
	Content    string
	ToolName   *string
	ToolCallID *string
	SourceIM   string     // "feishu", "cli", "cron"
	Channel    string     // specific chat/group within the IM
	SourceTS   *time.Time // timestamp from originating platform
	MsgType    string     // "text", "image", "audio", "file", "post"
	FilePaths  []string   // paths to saved media files
	SenderID   string     // user identity from source IM
	CreatedAt  time.Time

	// Queue fields — only meaningful for user messages in the pipeline.
	// NULL reply_status means the message is not part of the queue.
	ReplyStatus    *string // received / filtering / ready / processing / done
	ReplyChannelID string  // IM-abstract handle for updating the reply card
	TriggerMsgID   string  // IM trigger message ID (reply thread root)
}

// Store is the base interface — all implementations must support this.
type Store interface {
	SaveMessage(ctx context.Context, msg *StoredMessage) error
	RecentMessages(ctx context.Context, chatID string, limit int) ([]StoredMessage, error)
}

// SemanticStore adds vector search capabilities (PostgreSQL + pgvector).
type SemanticStore interface {
	Store
	SearchMessages(ctx context.Context, embedding []float32, chatID string, limit int) ([]StoredMessage, error)
	UnembeddedMessages(ctx context.Context, limit int) ([]StoredMessage, error)
	SetMessageEmbedding(ctx context.Context, messageID int64, embedding []float32) error
}

// PinnedMemoryStore manages pinned user memories.
type PinnedMemoryStore interface {
	SaveMemory(ctx context.Context, mem *Memory) error
	ListMemories(ctx context.Context, activeOnly bool) ([]Memory, error)
	SearchMemories(ctx context.Context, embedding []float32, limit int) ([]Memory, error)
	DeleteMemory(ctx context.Context, id int64) error
	SetMemoryEmbedding(ctx context.Context, memoryID int64, embedding []float32) error
}

type Memory struct {
	ID        int64
	Content   string
	Category  string // "preference", "fact", "instruction", "general"
	Active    bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// QueueStore manages per-chat reply serialization on the messages table.
// MQTT QoS=1 semantics: store first (received), ACK second (ready).
// Pipeline: received → filtering → ready → processing → done.
type QueueStore interface {
	// SaveMessageQueued inserts a user message with reply_status='received'.
	SaveMessageQueued(ctx context.Context, msg *StoredMessage) error
	// UpdateMessageContent overwrites the content field (Filter → processed text).
	UpdateMessageContent(ctx context.Context, msgID int64, content string) error
	// SetReplyStatus transitions a message to a new pipeline status.
	SetReplyStatus(ctx context.Context, msgID int64, status string) error
	// UpdateMessageFilePaths sets the file_paths array for a message.
	UpdateMessageFilePaths(ctx context.Context, msgID int64, paths []string) error
	// AckReply transitions to 'ready' and records the IM-abstract reply channel ID.
	AckReply(ctx context.Context, msgID int64, replyChannelID string) error
	// ClaimNextReply atomically picks the oldest 'ready' message for chatID
	// and marks it 'processing'. Returns nil if nothing queued.
	ClaimNextReply(ctx context.Context, chatID string) (*StoredMessage, error)
	// FinishReply marks a message as 'done'.
	FinishReply(ctx context.Context, msgID int64) error
	// RecoverQueue runs at startup: resets processing→ready, filtering→received.
	// Returns the number of rows recovered and any 'received' messages that need
	// Filter re-processing (thinking card was never sent).
	RecoverQueue(ctx context.Context) (recovered int, unacked []StoredMessage, err error)
	// PendingChats returns distinct chatIDs with 'ready' messages (for startup dispatch).
	PendingChats(ctx context.Context) ([]string, error)
}

// TraceStore records the full processing trace for each message.
type TraceStore interface {
	CreateTrace(ctx context.Context, t *Trace) error
	FinishTrace(ctx context.Context, t *Trace) error
	SaveToolCalls(ctx context.Context, calls []ToolCallRecord) error
}

type Trace struct {
	ID                    int64
	MessageID             int64
	ReplyID               int64
	TaskID                *int64
	ParentTaskID          *int64
	ChatID                string
	OrchestratorModel     string
	SynthesizerModel      string
	Iterations            int
	Summary               string
	TotalPromptTokens     int
	TotalCompletionTokens int
	SynthPromptTokens     int
	SynthCompletionTokens int
	DurationMs            int
	CreatedAt             time.Time
}

type ToolCallRecord struct {
	ID             int64
	TraceID        int64
	Iteration      int
	Seq            int
	ToolName       string
	Arguments      string // raw model-issued arguments
	NormalizedArgs string // deterministic parsed form (for skill induction)
	Result         string
	IsError        bool
	DurationMs     int
}

// TaskStore manages durable background tasks.
type TaskStore interface {
	CreateTask(ctx context.Context, task *Task) error
	GetTask(ctx context.Context, taskID int64) (*Task, error)
	ClaimNextTask(ctx context.Context, workerID string, leaseUntil time.Time) (*Task, error)
	CompleteTask(ctx context.Context, taskID int64, result string) error
	FailTask(ctx context.Context, taskID int64, errorMsg string) error
	CancelTask(ctx context.Context, taskID int64) (bool, error)
	RecoverExpiredTasks(ctx context.Context, now time.Time) (int, error)
}

type Task struct {
	ID               int64
	Kind             string // "sync" or "async"
	Source           string // "im", "cron", "agent"
	ChatID           string
	UserID           string
	ParentTaskID     *int64
	TriggerMessageID *int64
	Status           string // queued / running / succeeded / failed / cancelled
	Priority         int
	Title            string
	Input            []byte // JSON object
	Result           string
	Error            string
	LeaseOwner       string
	LeaseUntil       *time.Time
	CreatedAt        time.Time
	StartedAt        *time.Time
	FinishedAt       *time.Time
}

// OutboxStore manages deferred user-visible delivery events.
type OutboxStore interface {
	CreateOutboxEvent(ctx context.Context, event *OutboxEvent) error
	PendingOutboxChats(ctx context.Context) ([]string, error)
	ClaimNextOutboxEvent(ctx context.Context, chatID string) (*OutboxEvent, error)
	MarkOutboxSent(ctx context.Context, eventID int64) error
	MarkOutboxError(ctx context.Context, eventID int64, errorMsg string) error
	RecoverOutbox(ctx context.Context) (int, error)
}

type OutboxEvent struct {
	ID        int64
	ChatID    string
	TaskID    *int64
	Kind      string // async_done / async_failed / reminder / notice
	Payload   []byte // JSON object
	Status    string // pending / sending / sent
	Priority  int
	Error     string
	CreatedAt time.Time
	SentAt    *time.Time
}

// CronStore manages database-backed schedules that produce async tasks.
type CronStore interface {
	CreateCronSchedule(ctx context.Context, schedule *CronSchedule) error
	ListCronSchedules(ctx context.Context, chatID string, includeDisabled bool) ([]CronSchedule, error)
	DeleteCronSchedule(ctx context.Context, scheduleID int64, chatID string) (bool, error)
	DueCronSchedules(ctx context.Context, now time.Time, limit int) ([]CronSchedule, error)
	MarkCronScheduleRun(ctx context.Context, scheduleID int64, lastRunAt, nextRunAt time.Time) error
}

type CronSchedule struct {
	ID              int64
	ChatID          string
	UserID          string
	Name            string
	ScheduleType    string
	CronExpr        string
	Hour            int
	Minute          int
	Timezone        string
	Prompt          string
	Enabled         bool
	CreatedByTaskID *int64
	LastRunAt       *time.Time
	NextRunAt       *time.Time
	CreatedAt       time.Time
	UpdatedAt       time.Time
}

// DocumentStore manages document ingestion and RAG.
type DocumentStore interface {
	SaveDocument(ctx context.Context, doc *Document) error
	UpdateDocumentStatus(ctx context.Context, id int64, status, errorMsg string) error
	PendingDocuments(ctx context.Context, limit int) ([]Document, error)
	SaveChunks(ctx context.Context, chunks []Chunk) error
	SearchChunks(ctx context.Context, embedding []float32, limit int) ([]Chunk, error)
	ListDocuments(ctx context.Context) ([]Document, error)
	UnembeddedChunks(ctx context.Context, limit int) ([]Chunk, error)
	SetChunkEmbedding(ctx context.Context, chunkID int64, embedding []float32) error
}

type Document struct {
	ID         int64
	Filename   string
	FilePath   string
	Channel    string
	Status     string // "pending", "processing", "ready", "error"
	ErrorMsg   string
	ChunkCount int // populated by ListDocuments
	CreatedAt  time.Time
}

type Chunk struct {
	ID          int64
	DocumentID  int64
	ChunkIndex  int
	Content     string
	CreatedAt   time.Time
	Similarity  float64 // populated by search
	DocFilename string  // joined from documents
}
