package model

import "context"

// Client is the interface for LLM model clients.
type Client interface {
	Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error)
	ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamChunk, error)
	// ChatWithEarlyAbort opens a streaming request and returns a complete
	// Response. If the model produces more than maxTextTokens of content
	// text without emitting any tool calls, the stream is cancelled early
	// and the partial text is returned (Content set, ToolCalls empty).
	// This allows the orchestrator to detect and retry text-only responses
	// in ~1s instead of waiting 10-30s for full generation.
	ChatWithEarlyAbort(ctx context.Context, messages []Message, tools []ToolDef, maxTextTokens int) (*Response, error)
	Transcribe(ctx context.Context, audioData []byte, filename string) (*TranscriptionResult, error)
}

// StreamChunk is a single SSE chunk from a streaming chat completion.
type StreamChunk struct {
	Delta string // incremental content appended since the previous chunk
	Done  bool   // true on the final chunk; no more chunks follow
	Usage Usage  // set when Done if the server reports it
	Err   error  // set when Done if streaming errored
}
