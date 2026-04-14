package agent

import (
	"context"
	"fmt"
	"log/slog"

	"argus/internal/model"
	"argus/internal/skill"
	"argus/internal/store"
	"argus/internal/tool"
)

const maxToolResultBytes = 16 * 1024 // 16KB — prevents a single tool result from blowing up context

// Agent is the core agent loop with harness-based context assembly.
type Agent struct {
	model         model.Client
	store         store.Store
	toolRegistry  *tool.Registry
	skillIndex    *skill.SkillIndex
	basePrompt    string
	workspaceDir  string
	contextWindow int
	maxIterations int
}

func New(modelClient model.Client, st store.Store, toolReg *tool.Registry, skillIdx *skill.SkillIndex, basePrompt, workspaceDir string, contextWindow, maxIterations int) *Agent {
	if maxIterations == 0 {
		maxIterations = 10
	}
	return &Agent{
		model:         modelClient,
		store:         st,
		toolRegistry:  toolReg,
		skillIndex:    skillIdx,
		workspaceDir:  workspaceDir,
		basePrompt:    basePrompt,
		contextWindow: contextWindow,
		maxIterations: maxIterations,
	}
}

// Handle processes a user message and returns the assistant's reply.
// userMsg can have Content as string (text) or []ContentPart (multimodal).
func (a *Agent) Handle(ctx context.Context, chatID string, userMsg model.Message) (string, error) {
	// Inject chatID into context for tools (e.g. save_skill, db_exec).
	ctx = tool.WithChatID(ctx, chatID)

	// Save user message (store text content only).
	if err := a.store.SaveMessage(ctx, &store.StoredMessage{
		ChatID:  chatID,
		Role:    string(model.RoleUser),
		Content: userMsg.TextContent(),
	}); err != nil {
		return "", fmt.Errorf("save user message: %w", err)
	}

	// Assemble context via harness: skill selection + prompt assembly + history curation.
	messages, toolDefs, err := a.assembleContext(ctx, chatID, userMsg)
	if err != nil {
		return "", fmt.Errorf("assemble context: %w", err)
	}

	// Agent tool loop.
	for i := 0; i < a.maxIterations; i++ {
		slog.Info("calling model", "chat_id", chatID, "iteration", i, "messages", len(messages), "tools", len(toolDefs))

		resp, err := a.model.Chat(ctx, messages, toolDefs)
		if err != nil {
			return "", fmt.Errorf("model chat (iteration %d): %w", i, err)
		}

		slog.Info("model response",
			"iteration", i,
			"prompt_tokens", resp.Usage.PromptTokens,
			"completion_tokens", resp.Usage.CompletionTokens,
			"total_tokens", resp.Usage.TotalTokens,
		)

		// If there are tool calls, execute them regardless of finish_reason.
		// Only stop when the model produces no tool calls.
		if len(resp.ToolCalls) == 0 {
			reply := resp.Content

			// Save assistant reply.
			if err := a.store.SaveMessage(ctx, &store.StoredMessage{
				ChatID:  chatID,
				Role:    string(model.RoleAssistant),
				Content: reply,
			}); err != nil {
				return "", fmt.Errorf("save assistant reply: %w", err)
			}

			return reply, nil
		}

		// Append assistant message with tool calls.
		messages = append(messages, model.Message{
			Role:      model.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Execute each tool call and append results.
		for _, tc := range resp.ToolCalls {
			slog.Info("tool call",
				"tool", tc.Function.Name,
				"call_id", tc.ID,
				"arguments", tc.Function.Arguments,
			)

			result := a.executeTool(ctx, tc)
			result = truncateResult(result, maxToolResultBytes)

			slog.Info("tool result",
				"tool", tc.Function.Name,
				"call_id", tc.ID,
				"result_len", len(result),
			)

			messages = append(messages, model.Message{
				Role:       model.RoleTool,
				Content:    result,
				ToolCallID: tc.ID,
			})
		}
	}

	return "", fmt.Errorf("agent loop exceeded max iterations (%d)", a.maxIterations)
}

// executeTool runs a tool and returns the result as a string.
// Errors are returned as the result string so the model can see them.
func (a *Agent) executeTool(ctx context.Context, tc model.ToolCall) string {
	t, ok := a.toolRegistry.Get(tc.Function.Name)
	if !ok {
		return fmt.Sprintf("error: unknown tool %q", tc.Function.Name)
	}

	result, err := t.Execute(ctx, tc.Function.Arguments)
	if err != nil {
		return fmt.Sprintf("error: %v", err)
	}

	return result
}

// truncateResult caps tool output to maxBytes, appending a notice if truncated.
func truncateResult(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + fmt.Sprintf("\n\n... [truncated: output was %d bytes, showing first %d]", len(s), maxBytes)
}
