package model

import "context"

// Client is the interface for LLM model clients.
type Client interface {
	Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error)
	Transcribe(ctx context.Context, audioData []byte, filename string) (*TranscriptionResult, error)
}
