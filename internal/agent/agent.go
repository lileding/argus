package agent

import (
	"context"
	"encoding/json"
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

// toolCallRecord tracks a tool invocation and its result for Phase 2 synthesis.
type toolCallRecord struct {
	Name      string
	Arguments string
	Result    string
}

// HandleStream processes a user message in two phases: orchestration (tool calling)
// then synthesis (final reply generation). Returns a channel of events for UI updates.
func (a *Agent) HandleStream(ctx context.Context, chatID string, userMsg model.Message) <-chan Event {
	ch := make(chan Event, 16)

	go func() {
		defer close(ch)

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

		// Load history for context (used by both phases).
		history, err := a.loadHistory(ctx, chatID, savedMsg.ID)
		if err != nil {
			ch <- Event{Type: EventError, Payload: ErrorPayload{Err: err}}
			return
		}

		userText := userMsg.TextContent()

		// Phase 1: Orchestration — collect materials via tool calls.
		toolResults, finishSummary := a.runOrchestrator(ctx, ch, userMsg, userText, history)

		// Phase 2: Synthesis — compose final answer from materials.
		reply := a.runSynthesizer(ctx, userMsg, userText, history, toolResults, finishSummary)

		// Save assistant reply.
		if err := a.store.SaveMessage(ctx, &store.StoredMessage{
			ChatID:  chatID,
			Role:    string(model.RoleAssistant),
			Content: reply,
		}); err != nil {
			ch <- Event{Type: EventError, Payload: ErrorPayload{Err: fmt.Errorf("save assistant reply: %w", err)}}
			return
		}

		ch <- Event{Type: EventReply, Payload: ReplyPayload{Text: reply}}
	}()

	return ch
}

// runOrchestrator is Phase 1: loops model calls + tool execution until finish_task.
func (a *Agent) runOrchestrator(
	ctx context.Context,
	ch chan<- Event,
	userMsg model.Message,
	userText string,
	history []model.Message,
) (results []toolCallRecord, summary string) {
	// Pre-search: keyword-triggered search before model sees anything.
	var preSearchHint string
	if searchResult := a.preSearch(ctx, userText); searchResult != "" {
		preSearchHint = searchResult
		results = append(results, toolCallRecord{
			Name:      "search",
			Arguments: userText,
			Result:    searchResult,
		})
		ch <- Event{Type: EventToolCall, Payload: ToolCallPayload{Name: "search", Arguments: userText}}
		ch <- Event{Type: EventToolResult, Payload: ToolResultPayload{Name: "search", Result: truncateResult(searchResult, 200)}}
	}

	// Build orchestrator system prompt.
	sysPrompt := a.buildOrchestratorPrompt(preSearchHint)

	// Build initial messages: system + history + user message.
	messages := make([]model.Message, 0, len(history)+2)
	messages = append(messages, model.Message{Role: model.RoleSystem, Content: sysPrompt})
	messages = append(messages, history...)
	userMsg.Role = model.RoleUser
	messages = append(messages, userMsg)

	toolDefs := a.toolRegistry.AllToolDefs()
	recentCalls := make(map[string]int)

	for i := 0; i < a.maxIterations; i++ {
		slog.Info("orchestrator iteration", "iteration", i, "messages", len(messages), "tools", len(toolDefs))

		resp, err := a.model.Chat(ctx, messages, toolDefs)
		if err != nil {
			slog.Error("orchestrator chat failed", "err", err)
			summary = fmt.Sprintf("Orchestrator error: %v", err)
			return
		}

		slog.Info("orchestrator response",
			"iteration", i,
			"prompt_tokens", resp.Usage.PromptTokens,
			"completion_tokens", resp.Usage.CompletionTokens,
			"tool_calls", len(resp.ToolCalls),
		)

		// Retry once if first response has no tool calls (model ignored instruction).
		if len(resp.ToolCalls) == 0 && i == 0 {
			slog.Warn("orchestrator ignored tool-only rule, retrying with enforcement")
			messages = append(messages,
				model.Message{Role: model.RoleAssistant, Content: resp.Content},
				model.Message{Role: model.RoleUser, Content: "You MUST call a tool. Text output is ignored. Call search, fetch, read_file, or finish_task now."},
			)
			resp, err = a.model.Chat(ctx, messages, toolDefs)
			if err != nil {
				summary = fmt.Sprintf("Orchestrator retry error: %v", err)
				return
			}
		}

		// If still no tool calls, use model text as fallback summary and exit.
		if len(resp.ToolCalls) == 0 {
			slog.Warn("orchestrator produced no tool calls after retry, using text as summary")
			summary = resp.Content
			return
		}

		// Append assistant message with tool calls.
		messages = append(messages, model.Message{
			Role:      model.RoleAssistant,
			Content:   resp.Content,
			ToolCalls: resp.ToolCalls,
		})

		// Execute tools (or detect finish_task).
		for _, tc := range resp.ToolCalls {
			// Detect finish_task — signal to move to synthesis.
			if tc.Function.Name == "finish_task" {
				var args struct {
					Summary string `json:"summary"`
				}
				json.Unmarshal([]byte(tc.Function.Arguments), &args)
				summary = args.Summary
				slog.Info("orchestrator done", "summary", truncateResult(summary, 200))
				return
			}

			callKey := tc.Function.Name + ":" + tc.Function.Arguments
			recentCalls[callKey]++

			if recentCalls[callKey] > 2 {
				slog.Warn("duplicate tool call detected", "tool", tc.Function.Name)
				errResult := fmt.Sprintf("error: this exact call (%s) has been repeated %d times. Try a different approach or call finish_task.", tc.Function.Name, recentCalls[callKey])
				messages = append(messages, model.Message{Role: model.RoleTool, Content: errResult, ToolCallID: tc.ID})
				continue
			}

			ch <- Event{Type: EventToolCall, Payload: ToolCallPayload{
				Name: tc.Function.Name, Arguments: tc.Function.Arguments, CallID: tc.ID,
			}}

			slog.Info("tool call", "tool", tc.Function.Name, "arguments", tc.Function.Arguments)

			result := a.executeTool(ctx, tc)
			result = truncateResult(result, maxToolResultBytes)

			slog.Info("tool result", "tool", tc.Function.Name, "result_len", len(result))

			ch <- Event{Type: EventToolResult, Payload: ToolResultPayload{
				Name: tc.Function.Name, CallID: tc.ID,
				Result:  truncateResult(result, 200),
				IsError: strings.HasPrefix(result, "error:"),
			}}

			results = append(results, toolCallRecord{
				Name:      tc.Function.Name,
				Arguments: tc.Function.Arguments,
				Result:    result,
			})

			messages = append(messages, model.Message{Role: model.RoleTool, Content: result, ToolCallID: tc.ID})
		}
	}

	summary = fmt.Sprintf("(reached max %d iterations without finish_task)", a.maxIterations)
	return
}

// runSynthesizer is Phase 2: composes the final answer from orchestrator's materials.
func (a *Agent) runSynthesizer(
	ctx context.Context,
	userMsg model.Message,
	userText string,
	history []model.Message,
	toolResults []toolCallRecord,
	summary string,
) string {
	// Build synthesizer prompt with environment info.
	sysPrompt := a.buildSynthesizerPrompt()

	// Build materials section.
	var materials strings.Builder
	materials.WriteString("## Materials Collected by Orchestrator\n\n")
	if summary != "" {
		materials.WriteString("### Summary\n")
		materials.WriteString(summary)
		materials.WriteString("\n\n")
	}
	for i, r := range toolResults {
		materials.WriteString(fmt.Sprintf("### Tool Call #%d: %s\n", i+1, r.Name))
		materials.WriteString(fmt.Sprintf("Arguments: `%s`\n\n", r.Arguments))
		materials.WriteString("Result:\n```\n")
		materials.WriteString(r.Result)
		materials.WriteString("\n```\n\n")
	}
	if len(toolResults) == 0 && summary == "" {
		materials.WriteString("(No tool results — answer from conversation context alone.)\n")
	}

	// Messages: system + history + user + materials.
	messages := make([]model.Message, 0, len(history)+3)
	messages = append(messages, model.Message{Role: model.RoleSystem, Content: sysPrompt})
	messages = append(messages, history...)
	userMsg.Role = model.RoleUser
	messages = append(messages, userMsg)
	messages = append(messages, model.Message{Role: model.RoleSystem, Content: materials.String()})

	slog.Info("synthesizer call", "materials_len", materials.Len(), "history_len", len(history))

	resp, err := a.model.Chat(ctx, messages, nil) // no tools
	if err != nil {
		slog.Error("synthesizer chat failed", "err", err)
		return fmt.Sprintf("Error generating response: %v", err)
	}

	slog.Info("synthesizer response",
		"prompt_tokens", resp.Usage.PromptTokens,
		"completion_tokens", resp.Usage.CompletionTokens,
	)

	return resp.Content
}

// Handle is the synchronous compatibility wrapper.
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

// preSearch checks if the user message contains explicit search intent and
// proactively runs a search.
func (a *Agent) preSearch(ctx context.Context, text string) string {
	lower := strings.ToLower(text)

	searchTriggers := []string{
		"搜索", "搜一下", "查一下", "查询", "网上找", "互联网", "上网",
		"最新", "最近", "新闻", "今天", "现在",
		"search", "look up", "google", "find online",
		"latest", "recent", "news", "today",
	}

	hasIntent := false
	for _, trigger := range searchTriggers {
		if strings.Contains(lower, trigger) {
			hasIntent = true
			break
		}
	}

	if !hasIntent {
		return ""
	}

	searchTool, ok := a.toolRegistry.Get("search")
	if !ok {
		return ""
	}

	// Extract query by removing common prefixes.
	query := text
	for _, prefix := range []string{
		"搜索网络给我", "搜索网络", "搜索一下", "搜索", "搜一下",
		"查一下", "查询", "网上找", "帮我搜索", "帮我查",
		"search for ", "search ", "look up ", "google ",
	} {
		if idx := strings.Index(lower, prefix); idx >= 0 {
			query = text[idx+len(prefix):]
			break
		}
	}
	query = strings.TrimSpace(query)
	if query == "" {
		query = text
	}

	slog.Info("pre-search triggered", "query", query)
	result, err := searchTool.Execute(ctx, `{"query":"`+strings.ReplaceAll(query, `"`, `\"`)+`"}`)
	if err != nil {
		slog.Warn("pre-search failed", "err", err)
		return ""
	}

	return result
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
