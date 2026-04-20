package agent

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"argus/internal/store"
	"argus/internal/tool"
)

// executeWithTrace wraps execute with trace persistence. It tees the event
// stream: one path goes to the Frontend (via eventsCh), the other collects
// trace data for DB persistence.
func (a *Agent) executeWithTrace(ctx context.Context, task Task, payload Payload, eventsCh chan<- Event) {
	start := time.Now()

	// Create trace record.
	trace := &store.Trace{
		MessageID:         task.MsgID,
		ChatID:            task.ChatID,
		OrchestratorModel: a.orchestratorModel,
		SynthesizerModel:  a.synthesizerModel,
	}
	if ts, ok := a.store.(store.TraceStore); ok {
		if err := ts.CreateTrace(ctx, trace); err != nil {
			slog.Warn("trace: create", "err", err)
		}
	}

	// Internal event channel for the executor.
	rawCh := make(chan Event, 16)

	var toolCalls []store.ToolCallRecord
	toolArgs := map[[2]int]string{} // iteration:seq → arguments
	var composing *ComposingPayload
	var replyPayload *ReplyPayload

	// Tee goroutine: forward events to Frontend + collect trace data.
	teeDone := make(chan struct{})
	go func() {
		defer close(eventsCh)
		defer close(teeDone)
		for ev := range rawCh {
			switch ev.Type {
			case EventToolCall:
				p := ev.Payload.(ToolCallPayload)
				toolArgs[[2]int{p.Iteration, p.Seq}] = p.Arguments
			case EventToolResult:
				p := ev.Payload.(ToolResultPayload)
				rawArgs := toolArgs[[2]int{p.Iteration, p.Seq}]
				normalizedArgs := rawArgs
				if p.Name == "db" {
					var parsed struct{ Command string }
					if json.Unmarshal([]byte(rawArgs), &parsed) == nil && parsed.Command != "" {
						cmd, err := tool.ParseDBCommand(parsed.Command)
						if err == nil {
							normalizedArgs = cmd.Normalize()
						}
					}
				}
				toolCalls = append(toolCalls, store.ToolCallRecord{
					TraceID:        trace.ID,
					Iteration:      p.Iteration,
					Seq:            p.Seq,
					ToolName:       p.Name,
					Arguments:      rawArgs,
					NormalizedArgs: normalizedArgs,
					Result:         p.FullResult,
					IsError:        p.IsError,
					DurationMs:     p.DurationMs,
				})
			case EventComposing:
				if p, ok := ev.Payload.(ComposingPayload); ok {
					composing = &p
				}
			case EventReply:
				if p, ok := ev.Payload.(ReplyPayload); ok {
					replyPayload = &p
				}
			}
			eventsCh <- ev
		}
	}()

	// Run the actual execution.
	a.execute(ctx, task.ChatID, task.MsgID, payload, rawCh)
	close(rawCh) // signals tee goroutine to finish
	<-teeDone    // wait for tee to drain all events before reading trace data

	// Persist trace.
	if ts, ok := a.store.(store.TraceStore); ok && trace.ID > 0 {
		if composing != nil {
			trace.Iterations = composing.Iterations
			trace.Summary = composing.Summary
			trace.TotalPromptTokens = composing.TotalPromptTokens
			trace.TotalCompletionTokens = composing.TotalCompletionTokens
		}
		if replyPayload != nil {
			trace.ReplyID = replyPayload.ReplyMsgID
			trace.SynthPromptTokens = replyPayload.PromptTokens
			trace.SynthCompletionTokens = replyPayload.CompletionTokens
		}
		trace.DurationMs = int(time.Since(start).Milliseconds())

		if err := ts.FinishTrace(ctx, trace); err != nil {
			slog.Warn("trace: finish", "err", err)
		}
		if len(toolCalls) > 0 {
			if err := ts.SaveToolCalls(ctx, toolCalls); err != nil {
				slog.Warn("trace: save tool calls", "err", err)
			}
		}
		slog.Info("trace saved",
			"trace_id", trace.ID, "msg_id", task.MsgID,
			"iterations", trace.Iterations,
			"tool_calls", len(toolCalls),
			"duration_ms", trace.DurationMs,
		)
	}
}
