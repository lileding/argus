package tool

import (
	"context"
	"encoding/json"
)

// FinishTaskTool is a sentinel tool used by the orchestrator to signal task completion.
// It is never actually executed — the agent loop detects this tool call and transitions
// to the synthesis phase.
type FinishTaskTool struct{}

func NewFinishTaskTool() *FinishTaskTool { return &FinishTaskTool{} }

func (t *FinishTaskTool) Name() string { return "finish_task" }

func (t *FinishTaskTool) Description() string {
	return "Call this tool when you have collected enough information from other tools to answer the user's question. " +
		"Do NOT answer in text — text output is ignored. Just call this tool with a brief summary of what you found."
}

func (t *FinishTaskTool) Parameters() json.RawMessage {
	return json.RawMessage(`{
		"type": "object",
		"properties": {
			"summary": {
				"type": "string",
				"description": "Brief summary of findings and how they answer the user's question. The synthesizer will use this along with the full tool results to compose the final answer."
			}
		},
		"required": ["summary"]
	}`)
}

// Execute is never called — the agent loop intercepts this tool call.
// Returning empty keeps the Tool interface satisfied.
func (t *FinishTaskTool) Execute(_ context.Context, _ string) (string, error) {
	return "", nil
}
