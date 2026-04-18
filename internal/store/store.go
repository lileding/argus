package store

import (
	"context"
	"time"
)

// StoredMessage represents a message persisted in the store.
type StoredMessage struct {
	ID         int64
	ChatID     string     // composite key: "feishu:p2p:ou_xxx", "cli:local"
	Role       string     // user / assistant / tool
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
	ID                     int64
	MessageID              int64
	ReplyID                int64
	ChatID                 string
	Iterations             int
	Summary                string
	TotalPromptTokens      int
	TotalCompletionTokens  int
	SynthPromptTokens      int
	SynthCompletionTokens  int
	DurationMs             int
	CreatedAt              time.Time
}

type ToolCallRecord struct {
	ID         int64
	TraceID    int64
	Iteration  int
	Seq        int
	ToolName   string
	Arguments  string
	Result     string
	IsError    bool
	DurationMs int
}

// DocumentStore manages document ingestion and RAG.
type DocumentStore interface {
	SaveDocument(ctx context.Context, doc *Document) error
	UpdateDocumentStatus(ctx context.Context, id int64, status, errorMsg string) error
	PendingDocuments(ctx context.Context, limit int) ([]Document, error)
	SaveChunks(ctx context.Context, chunks []Chunk) error
	SearchChunks(ctx context.Context, embedding []float32, limit int) ([]Chunk, error)
	UnembeddedChunks(ctx context.Context, limit int) ([]Chunk, error)
	SetChunkEmbedding(ctx context.Context, chunkID int64, embedding []float32) error
}

type Document struct {
	ID        int64
	Filename  string
	FilePath  string
	Channel   string
	Status    string // "pending", "processing", "ready", "error"
	ErrorMsg  string
	CreatedAt time.Time
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
