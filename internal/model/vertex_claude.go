package model

import (
	"context"
	"fmt"
)

// vertexClaudeClient implements Client using the Anthropic SDK with
// Vertex AI authentication for Claude models.
type vertexClaudeClient struct {
	project   string
	location  string
	modelName string
	maxTokens int
}

func newVertexClaudeClient(ctx context.Context, project, location, modelName string, maxTokens int) (Client, error) {
	// TODO: initialize anthropic.Client with WithVertexAI
	return &vertexClaudeClient{
		project:   project,
		location:  location,
		modelName: modelName,
		maxTokens: maxTokens,
	}, nil
}

func (c *vertexClaudeClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error) {
	return nil, fmt.Errorf("vertex_ai claude Chat: not yet implemented")
}

func (c *vertexClaudeClient) ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamChunk, error) {
	return nil, fmt.Errorf("vertex_ai claude ChatStream: not yet implemented")
}

func (c *vertexClaudeClient) ChatWithEarlyAbort(ctx context.Context, messages []Message, tools []ToolDef, maxTextTokens int) (*Response, error) {
	return nil, fmt.Errorf("vertex_ai claude ChatWithEarlyAbort: not yet implemented")
}

func (c *vertexClaudeClient) Transcribe(ctx context.Context, audioData []byte, filename string) (*TranscriptionResult, error) {
	return nil, fmt.Errorf("vertex_ai does not support transcription (use openai upstream with Whisper)")
}
