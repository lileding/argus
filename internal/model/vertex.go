package model

import (
	"context"
	"fmt"
)

// NewVertexAIClient creates a Client for any model available on Vertex AI
// (Gemini, Claude, etc.) using the unified GenerateContent API via the
// Google genai SDK. No model-specific branching — all models share the
// same SDK and API format.
func NewVertexAIClient(ctx context.Context, project, location, modelName string, maxTokens int) (Client, error) {
	if project == "" || location == "" {
		return nil, fmt.Errorf("vertex_ai requires project and location")
	}
	return newVertexClient(ctx, project, location, modelName, maxTokens)
}
