package agent

import (
	"context"
	"fmt"
	"log/slog"
	"strings"

	"argus/internal/embedding"
	"argus/internal/model"
	"argus/internal/skill"
	"argus/internal/store"
	"argus/internal/tool"
)

const maxToolResultBytes = 16 * 1024

type Agent struct {
	model         model.Client
	store         store.Store
	toolRegistry  *tool.Registry
	skillIndex    *skill.SkillIndex
	embedder      *embedding.Client
	basePrompt    string
	workspaceDir  string
	contextWindow int
	maxIterations int
}

func New(modelClient model.Client, st store.Store, toolReg *tool.Registry, skillIdx *skill.SkillIndex, embedder *embedding.Client, basePrompt, workspaceDir string, contextWindow, maxIterations int) *Agent {
	if maxIterations == 0 {
		maxIterations = 10
	}
	return &Agent{
		model:         modelClient,
		store:         st,
		toolRegistry:  toolReg,
		skillIndex:    skillIdx,
		embedder:      embedder,
		workspaceDir:  workspaceDir,
		basePrompt:    basePrompt,
		contextWindow: contextWindow,
		maxIterations: maxIterations,
	}
}

// HandleStream processes a user message and returns a channel of events.
// The caller consumes events to drive UI updates (thinking, tool calls, final reply).
func (a *Agent) HandleStream(ctx context.Context, chatID string, userMsg model.Message) <-chan Event {
	ch := make(chan Event, 16)

	go func() {
		defer close(ch)

		// Emit thinking immediately.
		ch <- Event{Type: EventThinking, Payload: ThinkingPayload{UserText: userMsg.TextContent()}}

		ctx = tool.WithChatID(ctx, chatID)

		// Save user message (crash-safe).
		savedMsg := &store.StoredMessage{
			ChatID:  chatID,
			Role:    string(model.RoleUser),
			Content: userMsg.TextContent(),
		}
		if meta := userMsg.Meta; meta != nil {
			savedMsg.SourceIM = meta.SourceIM
			savedMsg.Channel = meta.Channel
			savedMsg.SourceTS = meta.SourceTS
			savedMsg.MsgType = meta.MsgType
			savedMsg.FilePaths = meta.FilePaths
			savedMsg.SenderID = meta.SenderID
		}
		if err := a.store.SaveMessage(ctx, savedMsg); err != nil {
			ch <- Event{Type: EventError, Payload: ErrorPayload{Err: fmt.Errorf("save user message: %w", err)}}
			return
		}

		// Assemble context.
		messages, toolDefs, err := a.assembleContext(ctx, chatID, userMsg, savedMsg.ID)
		if err != nil {
			ch <- Event{Type: EventError, Payload: ErrorPayload{Err: fmt.Errorf("assemble context: %w", err)}}
			return
		}

		// Agent tool loop.
		recentCalls := make(map[string]int)
		for i := 0; i < a.maxIterations; i++ {
			slog.Info("calling model", "chat_id", chatID, "iteration", i, "messages", len(messages), "tools", len(toolDefs))

			resp, err := a.model.Chat(ctx, messages, toolDefs)
			if err != nil {
				ch <- Event{Type: EventError, Payload: ErrorPayload{Err: fmt.Errorf("model chat (iteration %d): %w", i, err)}}
				return
			}

			slog.Info("model response",
				"iteration", i,
				"prompt_tokens", resp.Usage.PromptTokens,
				"completion_tokens", resp.Usage.CompletionTokens,
				"total_tokens", resp.Usage.TotalTokens,
			)

			// No tool calls → final reply.
			if len(resp.ToolCalls) == 0 {
				reply := resp.Content
				if err := a.store.SaveMessage(ctx, &store.StoredMessage{
					ChatID:  chatID,
					Role:    string(model.RoleAssistant),
					Content: reply,
				}); err != nil {
					ch <- Event{Type: EventError, Payload: ErrorPayload{Err: fmt.Errorf("save assistant reply: %w", err)}}
					return
				}
				ch <- Event{Type: EventReply, Payload: ReplyPayload{Text: reply}}
				return
			}

			// Append assistant message with tool calls.
			messages = append(messages, model.Message{
				Role:      model.RoleAssistant,
				Content:   resp.Content,
				ToolCalls: resp.ToolCalls,
			})

			// Execute tools.
			for _, tc := range resp.ToolCalls {
				callKey := tc.Function.Name + ":" + tc.Function.Arguments
				recentCalls[callKey]++

				if recentCalls[callKey] > 2 {
					slog.Warn("duplicate tool call detected", "tool", tc.Function.Name, "count", recentCalls[callKey])
					result := fmt.Sprintf("error: this exact call (%s) has been repeated %d times. Try a different approach.", tc.Function.Name, recentCalls[callKey])
					messages = append(messages, model.Message{Role: model.RoleTool, Content: result, ToolCallID: tc.ID})
					continue
				}

				// Emit tool call event.
				ch <- Event{Type: EventToolCall, Payload: ToolCallPayload{
					Name: tc.Function.Name, Arguments: tc.Function.Arguments, CallID: tc.ID,
				}}

				slog.Info("tool call", "tool", tc.Function.Name, "call_id", tc.ID, "arguments", tc.Function.Arguments)

				result := a.executeTool(ctx, tc)
				result = truncateResult(result, maxToolResultBytes)

				slog.Info("tool result", "tool", tc.Function.Name, "call_id", tc.ID, "result_len", len(result))

				// Emit tool result event.
				ch <- Event{Type: EventToolResult, Payload: ToolResultPayload{
					Name: tc.Function.Name, CallID: tc.ID,
					Result:  truncateResult(result, 200),
					IsError: strings.HasPrefix(result, "error:"),
				}}

				messages = append(messages, model.Message{Role: model.RoleTool, Content: result, ToolCallID: tc.ID})
			}
		}

		ch <- Event{Type: EventError, Payload: ErrorPayload{Err: fmt.Errorf("agent loop exceeded max iterations (%d)", a.maxIterations)}}
	}()

	return ch
}

// Handle is the synchronous compatibility wrapper. Blocks until the agent finishes.
func (a *Agent) Handle(ctx context.Context, chatID string, userMsg model.Message) (string, error) {
	ch := a.HandleStream(ctx, chatID, userMsg)
	var reply string
	var lastErr error
	for ev := range ch {
		switch ev.Type {
		case EventReply:
			reply = ev.Payload.(ReplyPayload).Text
		case EventError:
			lastErr = ev.Payload.(ErrorPayload).Err
		}
	}
	if lastErr != nil {
		return "", lastErr
	}
	return reply, nil
}

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

func truncateResult(s string, maxBytes int) string {
	if len(s) <= maxBytes {
		return s
	}
	return s[:maxBytes] + fmt.Sprintf("\n\n... [truncated: %d bytes, showing first %d]", len(s), maxBytes)
}
