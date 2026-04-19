package agent

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"argus/internal/embedding"
	"argus/internal/model"
	"argus/internal/skill"
	"argus/internal/store"
	"argus/internal/tool"
)

const (
	maxImagesPerMessage = 4       // max images injected per message
	maxImageBytes       = 1 << 20 // 1 MB per image file
)

// buildUserMessage creates a text or multimodal message from stored content.
// Limits: at most maxImagesPerMessage images, each at most maxImageBytes.
// Excess images get a text placeholder.
func buildUserMessage(text string, filePaths []string, workspaceDir string) model.Message {
	var dataURLs []string
	skipped := 0
	for _, p := range filePaths {
		ext := strings.ToLower(filepath.Ext(p))
		if !imageExts[ext] {
			continue
		}
		if len(dataURLs) >= maxImagesPerMessage {
			skipped++
			continue
		}
		absPath := filepath.Join(workspaceDir, p)
		info, err := os.Stat(absPath)
		if err != nil {
			continue
		}
		if info.Size() > maxImageBytes {
			slog.Info("skip oversized image", "path", p, "size", info.Size())
			skipped++
			continue
		}
		data, err := os.ReadFile(absPath)
		if err != nil {
			continue
		}
		contentType := http.DetectContentType(data)
		dataURLs = append(dataURLs, fmt.Sprintf("data:%s;base64,%s",
			contentType, base64.StdEncoding.EncodeToString(data)))
	}
	if skipped > 0 {
		text += fmt.Sprintf("\n[%d image(s) omitted: exceeded size or count limit]", skipped)
	}
	if len(dataURLs) > 0 {
		return model.NewMultimodalMessage(model.RoleUser, text, dataURLs...)
	}
	return model.NewTextMessage(model.RoleUser, text)
}

const maxToolResultBytes = 16 * 1024

type Agent struct {
	orchestrator              model.Client // Phase 1: tool calling
	synthesizer               model.Client // Phase 2: answer generation
	store                     store.Store
	toolRegistry              *tool.Registry
	skillIndex                *skill.SkillIndex
	embedder                  *embedding.Client
	workspaceDir              string
	contextWindow             int // synthesizer history window
	orchestratorContextWindow int // orchestrator history window (smaller)
	maxIterations             int
}

func New(orchestrator, synthesizer model.Client, st store.Store, toolReg *tool.Registry, skillIdx *skill.SkillIndex, embedder *embedding.Client, workspaceDir string, contextWindow, orchestratorContextWindow, maxIterations int) *Agent {
	if maxIterations == 0 {
		maxIterations = 10
	}
	return &Agent{
		orchestrator:              orchestrator,
		synthesizer:               synthesizer,
		store:                     st,
		toolRegistry:              toolReg,
		skillIndex:                skillIdx,
		embedder:                  embedder,
		workspaceDir:              workspaceDir,
		contextWindow:             contextWindow,
		orchestratorContextWindow: orchestratorContextWindow,
		maxIterations:             maxIterations,
	}
}

// toolCallRecord tracks a tool invocation and its result for Phase 2 synthesis.
type toolCallRecord struct {
	Name      string
	Arguments string
	Result    string
}

// orchestratorStats captures metrics from Phase 1 for trace recording.
type orchestratorStats struct {
	Iterations            int
	TotalPromptTokens     int
	TotalCompletionTokens int
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

		userText := userMsg.TextContent()

		// Phase 1: Orchestration — smaller context window to save tokens.
		orchHistory, err := a.loadHistory(ctx, chatID, savedMsg.ID, a.orchestratorContextWindow)
		if err != nil {
			ch <- Event{Type: EventError, Payload: ErrorPayload{Err: err}}
			return
		}
		toolResults, finishSummary, orchStats := a.runOrchestrator(ctx, ch, userMsg, userText, orchHistory)

		// Signal transition with orchestrator stats for trace recording.
		ch <- Event{Type: EventComposing, Payload: ComposingPayload{
			Iterations:            orchStats.Iterations,
			Summary:               finishSummary,
			TotalPromptTokens:     orchStats.TotalPromptTokens,
			TotalCompletionTokens: orchStats.TotalCompletionTokens,
		}}

		// Phase 2: Synthesis — full context window for richer answers.
		synthHistory, err := a.loadHistory(ctx, chatID, savedMsg.ID, a.contextWindow)
		if err != nil {
			ch <- Event{Type: EventError, Payload: ErrorPayload{Err: err}}
			return
		}
		reply, synthPT, synthCT := a.runSynthesizer(ctx, ch, userMsg, userText, synthHistory, toolResults, finishSummary)

		// Save assistant reply.
		savedReply := &store.StoredMessage{
			ChatID:  chatID,
			Role:    string(model.RoleAssistant),
			Content: reply,
		}
		if err := a.store.SaveMessage(ctx, savedReply); err != nil {
			ch <- Event{Type: EventError, Payload: ErrorPayload{Err: fmt.Errorf("save assistant reply: %w", err)}}
			return
		}

		ch <- Event{Type: EventReply, Payload: ReplyPayload{
			Text: reply, ReplyMsgID: savedReply.ID,
			PromptTokens: synthPT, CompletionTokens: synthCT,
		}}
	}()

	return ch
}

// HandleStreamQueued is the dispatcher entry point for pre-saved messages.
// Unlike HandleStream, it does NOT save the user message (already in the DB)
// and reconstructs the model.Message from the stored text content.
func (a *Agent) HandleStreamQueued(ctx context.Context, chatID string, savedMsgID int64, userText string, filePaths []string) <-chan Event {
	ch := make(chan Event, 16)

	go func() {
		defer close(ch)

		ctx = tool.WithChatID(ctx, chatID)

		// Reconstruct message — multimodal if images are present.
		userMsg := buildUserMessage(userText, filePaths, a.workspaceDir)

		// Phase 1: Orchestration — smaller context window.
		orchHistory, err := a.loadHistory(ctx, chatID, savedMsgID, a.orchestratorContextWindow)
		if err != nil {
			ch <- Event{Type: EventError, Payload: ErrorPayload{Err: err}}
			return
		}
		toolResults, finishSummary, orchStats := a.runOrchestrator(ctx, ch, userMsg, userText, orchHistory)

		// Signal transition.
		ch <- Event{Type: EventComposing, Payload: ComposingPayload{
			Iterations:            orchStats.Iterations,
			Summary:               finishSummary,
			TotalPromptTokens:     orchStats.TotalPromptTokens,
			TotalCompletionTokens: orchStats.TotalCompletionTokens,
		}}

		// Phase 2: Synthesis — full context window.
		synthHistory, err := a.loadHistory(ctx, chatID, savedMsgID, a.contextWindow)
		if err != nil {
			ch <- Event{Type: EventError, Payload: ErrorPayload{Err: err}}
			return
		}
		reply, synthPT, synthCT := a.runSynthesizer(ctx, ch, userMsg, userText, synthHistory, toolResults, finishSummary)

		// Save assistant reply.
		savedReply := &store.StoredMessage{
			ChatID:  chatID,
			Role:    string(model.RoleAssistant),
			Content: reply,
		}
		if err := a.store.SaveMessage(ctx, savedReply); err != nil {
			ch <- Event{Type: EventError, Payload: ErrorPayload{Err: fmt.Errorf("save assistant reply: %w", err)}}
			return
		}

		ch <- Event{Type: EventReply, Payload: ReplyPayload{
			Text: reply, ReplyMsgID: savedReply.ID,
			PromptTokens: synthPT, CompletionTokens: synthCT,
		}}
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
) (results []toolCallRecord, summary string, stats orchestratorStats) {
	// Build orchestrator system prompt.
	sysPrompt := a.buildOrchestratorPrompt()

	// Build initial messages: system + history + user message.
	messages := make([]model.Message, 0, len(history)+2)
	messages = append(messages, model.Message{Role: model.RoleSystem, Content: sysPrompt})
	messages = append(messages, history...)
	userMsg.Role = model.RoleUser
	messages = append(messages, userMsg)

	toolDefs := a.toolRegistry.AllToolDefs()

	// Per-tool hard budgets. Once exhausted, further calls to that tool are
	// rejected at the harness layer (never dispatched to the actual tool).
	// Small budgets for expensive/noisy tools like search; unrestricted tools
	// (not listed) can be called freely.
	toolBudgets := map[string]int{
		"search":     3,
		"fetch":      4,
		"db":         6,
		"cli":        5,
		"write_file": 3,
		"remember":   3,
	}
	toolCounts := map[string]int{}

	// Cumulative count of calls rejected by budget. Models that ignore
	// "call finish_task" nudges keep issuing rejected calls; after too many
	// we force a transition to synthesis with whatever we already have.
	budgetRejections := 0
	const maxBudgetRejections = 5

	for i := 0; i < a.maxIterations; i++ {
		slog.Info("orchestrator iteration", "iteration", i, "messages", len(messages), "tools", len(toolDefs))

		resp, err := a.orchestrator.ChatWithEarlyAbort(ctx, messages, toolDefs, 80)
		if err != nil {
			slog.Error("orchestrator chat failed", "err", err)
			summary = fmt.Sprintf("Orchestrator error: %v", err)
			return
		}

		stats.Iterations = i + 1
		stats.TotalPromptTokens += resp.Usage.PromptTokens
		stats.TotalCompletionTokens += resp.Usage.CompletionTokens

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
			resp, err = a.orchestrator.ChatWithEarlyAbort(ctx, messages, toolDefs, 80)
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

		// Pre-scan for finish_task — if present, exit immediately.
		for _, tc := range resp.ToolCalls {
			if tc.Function.Name == "finish_task" {
				var args struct {
					Summary string `json:"summary"`
				}
				json.Unmarshal([]byte(tc.Function.Arguments), &args)
				summary = args.Summary
				slog.Info("orchestrator done", "summary", truncateResult(summary, 200))
				return
			}
		}

		// Pre-check budgets (serial — modifies shared state) and build
		// the list of tools to actually execute.
		type toolSlot struct {
			tc         model.ToolCall
			rejected   bool
			errMsg     string
			result     string
			fullResult string
			durationMs int
			seq        int // stable index for trace pairing
		}
		slots := make([]toolSlot, len(resp.ToolCalls))
		forceStop := false
		for i, tc := range resp.ToolCalls {
			slots[i].tc = tc
			if budget, has := toolBudgets[tc.Function.Name]; has {
				if toolCounts[tc.Function.Name] >= budget {
					budgetRejections++
					slots[i].rejected = true
					slots[i].errMsg = fmt.Sprintf(
						"error: %s budget exhausted (%d/%d calls already used). "+
							"Call finish_task NOW with a summary.",
						tc.Function.Name, toolCounts[tc.Function.Name], budget)
					slog.Warn("tool budget exhausted",
						"tool", tc.Function.Name,
						"rejections", budgetRejections,
					)
					if budgetRejections >= maxBudgetRejections {
						forceStop = true
					}
					continue
				}
				toolCounts[tc.Function.Name]++
			}
		}

		// Assign a stable seq to each slot (used by both ToolCall and ToolResult
		// events so dispatcher can pair them for trace recording).
		for idx := range slots {
			slots[idx].seq = idx
		}

		// Emit EventToolCall for non-rejected tools.
		for idx := range slots {
			if !slots[idx].rejected {
				ch <- Event{Type: EventToolCall, Payload: ToolCallPayload{
					Name: slots[idx].tc.Function.Name, Arguments: slots[idx].tc.Function.Arguments,
					CallID: slots[idx].tc.ID, Iteration: i, Seq: idx,
				}}
			}
		}

		// Execute non-rejected tools in parallel.
		var wg sync.WaitGroup
		for idx := range slots {
			if slots[idx].rejected {
				continue
			}
			wg.Add(1)
			go func(s *toolSlot) {
				defer wg.Done()
				slog.Info("tool call", "tool", s.tc.Function.Name, "arguments", s.tc.Function.Arguments)
				start := time.Now()
				raw := a.executeTool(ctx, s.tc)
				s.durationMs = int(time.Since(start).Milliseconds())
				s.fullResult = raw
				s.result = truncateResult(raw, maxToolResultBytes)
				slog.Info("tool result", "tool", s.tc.Function.Name, "result_len", len(s.result), "duration_ms", s.durationMs)
			}(&slots[idx])
		}
		wg.Wait()

		// Append results to messages and records in original order.
		for idx := range slots {
			s := &slots[idx]
			if s.rejected {
				ch <- Event{Type: EventToolResult, Payload: ToolResultPayload{
					Name: s.tc.Function.Name, CallID: s.tc.ID,
					Result: truncateResult(s.errMsg, 200), FullResult: s.errMsg,
					IsError: true, Iteration: i, Seq: s.seq,
				}}
				messages = append(messages, model.Message{Role: model.RoleTool, Content: s.errMsg, ToolCallID: s.tc.ID, ToolName: s.tc.Function.Name})
			} else {
				ch <- Event{Type: EventToolResult, Payload: ToolResultPayload{
					Name: s.tc.Function.Name, CallID: s.tc.ID,
					Result:     truncateResult(s.result, 200),
					FullResult: s.fullResult,
					IsError:    strings.HasPrefix(s.result, "error:"),
					DurationMs: s.durationMs,
					Iteration:  i, Seq: s.seq,
				}}
				results = append(results, toolCallRecord{
					Name:      s.tc.Function.Name,
					Arguments: s.tc.Function.Arguments,
					Result:    s.result,
				})
				messages = append(messages, model.Message{Role: model.RoleTool, Content: s.result, ToolCallID: s.tc.ID, ToolName: s.tc.Function.Name})
			}
		}

		if forceStop {
			slog.Warn("too many budget rejections, forcing synthesis",
				"rejections", budgetRejections, "results_gathered", len(results))
			summary = fmt.Sprintf("(Orchestrator force-stopped after %d budget rejections.)", budgetRejections)
			return
		}
	}

	// Max iterations reached. Don't fabricate a summary — let the synthesizer
	// compose an answer from whatever materials were actually gathered.
	slog.Warn("orchestrator reached max iterations without finish_task",
		"iterations", a.maxIterations,
		"results_gathered", len(results),
	)
	summary = fmt.Sprintf("(Orchestrator reached max %d iterations; synthesizing from %d materials gathered.)", a.maxIterations, len(results))
	return
}

// runSynthesizer is Phase 2: composes the final answer from orchestrator's materials.
// Streams the model's output as EventReplyDelta events (accumulated text).
func (a *Agent) runSynthesizer(
	ctx context.Context,
	ch chan<- Event,
	userMsg model.Message,
	userText string,
	history []model.Message,
	toolResults []toolCallRecord,
	summary string,
) (text string, promptTokens, completionTokens int) {
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

	// Messages: system + history + user + materials (as user, not system —
	// Anthropic/Gemini only support one system message).
	messages := make([]model.Message, 0, len(history)+3)
	messages = append(messages, model.Message{Role: model.RoleSystem, Content: sysPrompt})
	messages = append(messages, history...)
	userMsg.Role = model.RoleUser
	messages = append(messages, userMsg)
	messages = append(messages, model.Message{Role: model.RoleUser, Content: materials.String()})

	slog.Info("synthesizer call", "materials_len", materials.Len(), "history_len", len(history))

	stream, err := a.synthesizer.ChatStream(ctx, messages, nil) // no tools
	if err != nil {
		slog.Error("synthesizer stream start failed", "err", err)
		// Fallback to non-streaming.
		resp, fallbackErr := a.synthesizer.Chat(ctx, messages, nil)
		if fallbackErr != nil {
			return fmt.Sprintf("Error generating response: %v", fallbackErr), 0, 0
		}
		return resp.Content, resp.Usage.PromptTokens, resp.Usage.CompletionTokens
	}

	var full strings.Builder
	var finalUsage model.Usage
	for chunk := range stream {
		if chunk.Delta != "" {
			full.WriteString(chunk.Delta)
			ch <- Event{Type: EventReplyDelta, Payload: ReplyDeltaPayload{Text: full.String()}}
		}
		if chunk.Done {
			finalUsage = chunk.Usage
			if chunk.Err != nil {
				slog.Error("synthesizer stream error", "err", chunk.Err)
				if full.Len() == 0 {
					return fmt.Sprintf("Error generating response: %v", chunk.Err), 0, 0
				}
			}
		}
	}

	slog.Info("synthesizer response",
		"prompt_tokens", finalUsage.PromptTokens,
		"completion_tokens", finalUsage.CompletionTokens,
		"content_len", full.Len(),
	)

	return full.String(), finalUsage.PromptTokens, finalUsage.CompletionTokens
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
