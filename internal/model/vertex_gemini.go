package model

import (
	"context"
	"fmt"
)

// vertexGeminiClient implements Client using the Google genai SDK for
// Gemini models on Vertex AI.
type vertexGeminiClient struct {
	project   string
	location  string
	modelName string
	maxTokens int
}

func newVertexGeminiClient(ctx context.Context, project, location, modelName string, maxTokens int) (Client, error) {
	// TODO: initialize genai.Client
	return &vertexGeminiClient{
		project:   project,
		location:  location,
		modelName: modelName,
		maxTokens: maxTokens,
	}, nil
}

func (c *vertexGeminiClient) Chat(ctx context.Context, messages []Message, tools []ToolDef) (*Response, error) {
	return nil, fmt.Errorf("vertex_ai gemini Chat: not yet implemented")
}

func (c *vertexGeminiClient) ChatStream(ctx context.Context, messages []Message, tools []ToolDef) (<-chan StreamChunk, error) {
	return nil, fmt.Errorf("vertex_ai gemini ChatStream: not yet implemented")
}

func (c *vertexGeminiClient) ChatWithEarlyAbort(ctx context.Context, messages []Message, tools []ToolDef, maxTextTokens int) (*Response, error) {
	return nil, fmt.Errorf("vertex_ai gemini ChatWithEarlyAbort: not yet implemented")
}

func (c *vertexGeminiClient) Transcribe(ctx context.Context, audioData []byte, filename string) (*TranscriptionResult, error) {
	return nil, fmt.Errorf("vertex_ai does not support transcription (use openai upstream with Whisper)")
}
