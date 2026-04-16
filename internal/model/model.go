package model

import "context"

// Client is the interface for LLM model clients.
type Client interface {
	Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error)
	ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamChunk, error)
	Transcribe(ctx context.Context, audioData []byte, filename string) (*TranscriptionResult, error)
}

// StreamChunk is a single SSE chunk from a streaming chat completion.
type StreamChunk struct {
	Delta string // incremental content appended since the previous chunk
	Done  bool   // true on the final chunk; no more chunks follow
	Usage Usage  // set when Done if the server reports it
	Err   error  // set when Done if streaming errored
}
