package model

import (
	"context"
	"fmt"
	"strings"
)

// NewVertexAIClient creates a Client for Vertex AI, routing to the
// appropriate sub-implementation based on model name prefix:
//   - claude-* → Anthropic SDK with Vertex AI backend
//   - gemini-* → Google genai SDK
func NewVertexAIClient(ctx context.Context, project, location, modelName string, maxTokens int) (Client, error) {
	if project == "" || location == "" {
		return nil, fmt.Errorf("vertex_ai requires project and location")
	}

	if strings.HasPrefix(modelName, "claude-") {
		return newVertexClaudeClient(ctx, project, location, modelName, maxTokens)
	}
	if strings.HasPrefix(modelName, "gemini-") {
		return newVertexGeminiClient(ctx, project, location, modelName, maxTokens)
	}
	return nil, fmt.Errorf("unknown vertex_ai model prefix: %q (expected claude-* or gemini-*)", modelName)
}
