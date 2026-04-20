package store

import "context"

// RepairableStore can detect and fix data inconsistencies on startup.
type RepairableStore interface {
	RepairStuckDocuments(ctx context.Context) (int, error)
	RepairOrphanChunks(ctx context.Context) (int, error)
	CountUnembeddedMessages(ctx context.Context) (int, error)
	// FailedTranscriptions returns messages with transcription failure markers
	// that have audio files which could be re-transcribed.
	FailedTranscriptions(ctx context.Context) ([]StoredMessage, error)
	// CountUnsummarizedMessages returns the number of long assistant replies
	// that still need async summary generation.
	CountUnsummarizedMessages(ctx context.Context) (int, error)
}
