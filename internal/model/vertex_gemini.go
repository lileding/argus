package model

import (
	"context"
	"fmt"
)

// vertexClient implements Client using the Google genai SDK for all
// models on Vertex AI (Gemini, Claude, etc.) via the unified
// GenerateContent API.
type vertexClient struct {
	project   string
	location  string
	modelName string
	maxTokens int
	// TODO: genai.Client will be stored here after SDK integration
}

func newVertexClient(ctx context.Context, project, location, modelName string, maxTokens int) (Client, error) {
	// TODO: initialize genai.Client
	return &vertexClient{
		project:   project,
		location:  location,
		modelName: modelName,
		maxTokens: maxTokens,
	}, nil
}

func (c *vertexClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error) {
	return nil, fmt.Errorf("vertex_ai Chat: not yet implemented (model=%s)", c.modelName)
}

func (c *vertexClient) ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamChunk, error) {
	return nil, fmt.Errorf("vertex_ai ChatStream: not yet implemented (model=%s)", c.modelName)
}

func (c *vertexClient) ChatWithEarlyAbort(ctx context.Context, messages []Message, tools []ToolDef, maxTextTokens int) (*Response, error) {
	return nil, fmt.Errorf("vertex_ai ChatWithEarlyAbort: not yet implemented (model=%s)", c.modelName)
}

func (c *vertexClient) Transcribe(ctx context.Context, audioData []byte, filename string) (*TranscriptionResult, error) {
	return nil, fmt.Errorf("vertex_ai does not support transcription (use openai upstream with Whisper)")
}
