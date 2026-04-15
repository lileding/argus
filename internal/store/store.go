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
