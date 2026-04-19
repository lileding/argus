package task

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"argus/internal/agent"
	"argus/internal/store"
	"argus/internal/tool"
)

type Worker struct {
	store         store.TaskStore
	messageStore  store.Store
	traceStore    store.TraceStore
	outbox        store.OutboxStore
	agent         *agent.Agent
	workerID      string
	orchModel     string
	synthModel    string
	pollInterval  time.Duration
	leaseDuration time.Duration
	quit          chan struct{}
	wg            sync.WaitGroup
}

func NewWorker(st store.TaskStore, ag *agent.Agent, workerID string, pollInterval, leaseDuration time.Duration) *Worker {
	if workerID == "" {
		workerID = "task-worker"
	}
	if pollInterval == 0 {
		pollInterval = 2 * time.Second
	}
	if leaseDuration == 0 {
		leaseDuration = 30 * time.Minute
	}
	return &Worker{
		store:         st,
		agent:         ag,
		workerID:      workerID,
		pollInterval:  pollInterval,
		leaseDuration: leaseDuration,
		quit:          make(chan struct{}),
	}
}

func (w *Worker) WithOutbox(outbox store.OutboxStore) *Worker {
	w.outbox = outbox
	return w
}

func (w *Worker) WithMessageStore(messageStore store.Store) *Worker {
	w.messageStore = messageStore
	return w
}

func (w *Worker) WithTraceStore(traceStore store.TraceStore) *Worker {
	w.traceStore = traceStore
	return w
}

func (w *Worker) WithModelNames(orchestrator, synthesizer string) *Worker {
	w.orchModel = orchestrator
	w.synthModel = synthesizer
	return w
}

func (w *Worker) Start() {
	w.wg.Add(1)
	go w.run()
	slog.Info("async task worker started", "worker_id", w.workerID, "poll_interval", w.pollInterval)
}

func (w *Worker) Stop() {
	close(w.quit)
	w.wg.Wait()
	slog.Info("async task worker stopped", "worker_id", w.workerID)
}

func (w *Worker) run() {
	defer w.wg.Done()

	ticker := time.NewTicker(w.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			w.processAvailable()
		case <-w.quit:
			return
		}
	}
}

func (w *Worker) processAvailable() {
	ctx := context.Background()
	for {
		task, err := w.store.ClaimNextTask(ctx, w.workerID, time.Now().Add(w.leaseDuration))
		if err != nil {
			slog.Warn("claim async task", "err", err)
			return
		}
		if task == nil {
			return
		}
		w.processOne(ctx, task)
	}
}

func (w *Worker) processOne(ctx context.Context, task *store.Task) {
	slog.Info("async task running", "task_id", task.ID, "title", task.Title, "chat_id", task.ChatID)

	prompt, err := taskPrompt(task.Input)
	if err != nil {
		w.fail(ctx, task, err)
		return
	}
	if w.messageStore == nil {
		w.fail(ctx, task, fmt.Errorf("async task worker missing message store"))
		return
	}

	msg := &store.StoredMessage{
		ChatID:   task.ChatID,
		Role:     "user",
		Content:  prompt,
		SourceIM: "task",
		Channel:  task.ChatID,
		MsgType:  "text",
	}
	if err := w.messageStore.SaveMessage(ctx, msg); err != nil {
		w.fail(ctx, task, fmt.Errorf("save task message: %w", err))
		return
	}

	reply, err := w.runAgentWithTrace(ctx, task, msg)
	if err != nil {
		w.fail(ctx, task, err)
		return
	}
	if err := w.store.CompleteTask(ctx, task.ID, reply); err != nil {
		slog.Warn("complete async task", "task_id", task.ID, "err", err)
		return
	}
	w.enqueueOutbox(ctx, task, "async_done", map[string]string{
		"title":  task.Title,
		"result": reply,
	})

	slog.Info("async task succeeded", "task_id", task.ID)
}

func (w *Worker) runAgentWithTrace(ctx context.Context, task *store.Task, msg *store.StoredMessage) (string, error) {
	start := time.Now()
	trace := &store.Trace{
		MessageID:         msg.ID,
		TaskID:            &task.ID,
		ParentTaskID:      task.ParentTaskID,
		ChatID:            task.ChatID,
		OrchestratorModel: w.orchModel,
		SynthesizerModel:  w.synthModel,
	}
	if w.traceStore != nil {
		if err := w.traceStore.CreateTrace(ctx, trace); err != nil {
			slog.Warn("async task create trace", "task_id", task.ID, "err", err)
		}
	}

	agentCh := w.agent.HandleStreamQueued(ctx, task.ChatID, msg.ID, msg.Content, msg.FilePaths)
	var toolCalls []store.ToolCallRecord
	toolArgs := map[[2]int]string{}
	var composing *agent.ComposingPayload
	var replyPayload *agent.ReplyPayload
	var lastErr error

	for ev := range agentCh {
		switch ev.Type {
		case agent.EventToolCall:
			p := ev.Payload.(agent.ToolCallPayload)
			toolArgs[[2]int{p.Iteration, p.Seq}] = p.Arguments
		case agent.EventToolResult:
			p := ev.Payload.(agent.ToolResultPayload)
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
		case agent.EventComposing:
			if p, ok := ev.Payload.(agent.ComposingPayload); ok {
				composing = &p
			}
		case agent.EventReply:
			if p, ok := ev.Payload.(agent.ReplyPayload); ok {
				replyPayload = &p
			}
		case agent.EventError:
			if p, ok := ev.Payload.(agent.ErrorPayload); ok {
				lastErr = p.Err
			}
		}
	}

	if w.traceStore != nil && trace.ID > 0 {
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

		if err := w.traceStore.FinishTrace(ctx, trace); err != nil {
			slog.Warn("async task finish trace", "task_id", task.ID, "err", err)
		}
		if len(toolCalls) > 0 {
			if err := w.traceStore.SaveToolCalls(ctx, toolCalls); err != nil {
				slog.Warn("async task save tool calls", "task_id", task.ID, "err", err)
			}
		}
	}

	if lastErr != nil {
		return "", lastErr
	}
	if replyPayload == nil {
		return "", fmt.Errorf("agent produced no reply")
	}
	return replyPayload.Text, nil
}

func (w *Worker) fail(ctx context.Context, task *store.Task, err error) {
	slog.Warn("async task failed", "task_id", task.ID, "err", err)
	if failErr := w.store.FailTask(ctx, task.ID, err.Error()); failErr != nil {
		slog.Warn("mark async task failed", "task_id", task.ID, "err", failErr)
	}
	w.enqueueOutbox(ctx, task, "async_failed", map[string]string{
		"title": task.Title,
		"error": err.Error(),
	})
}

func (w *Worker) enqueueOutbox(ctx context.Context, task *store.Task, kind string, payload map[string]string) {
	if w.outbox == nil || task.ChatID == "" {
		return
	}
	data, err := json.Marshal(payload)
	if err != nil {
		slog.Warn("marshal async task outbox payload", "task_id", task.ID, "err", err)
		return
	}
	event := &store.OutboxEvent{
		ChatID:   task.ChatID,
		TaskID:   &task.ID,
		Kind:     kind,
		Payload:  data,
		Priority: task.Priority,
	}
	if err := w.outbox.CreateOutboxEvent(ctx, event); err != nil {
		slog.Warn("create async task outbox event", "task_id", task.ID, "err", err)
	}
}

func taskPrompt(input []byte) (string, error) {
	var args struct {
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(input, &args); err != nil {
		return "", fmt.Errorf("parse task input: %w", err)
	}
	if args.Prompt == "" {
		return "", fmt.Errorf("task input missing prompt")
	}
	return args.Prompt, nil
}
