package agent

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"argus/internal/store"
)

// SubmitTask routes a task to the per-chat FIFO queue.
// If this is the first task for the chat, a consumer goroutine is spawned.
func (a *Agent) SubmitTask(task Task) {
	ch := a.chatChan(task.ChatID)
	ch <- task
}

// SetContext sets the root context for the scheduler. Must be called
// before Recover or SubmitTask so that all goroutines inherit the
// correct cancellation context.
func (a *Agent) SetContext(ctx context.Context) {
	a.ctx = ctx
}

// Run blocks until ctx is cancelled, then performs graceful shutdown.
func (a *Agent) Run(ctx context.Context) error {
	a.ctx = ctx
	<-ctx.Done()
	a.Stop()
	return nil
}

// Stop closes all per-chat channels and waits for consumer goroutines.
// Idempotent — safe to call multiple times (Run + defer).
func (a *Agent) Stop() {
	a.stopOnce.Do(func() {
		a.chatChans.Range(func(key, value any) bool {
			close(value.(chan Task))
			return true
		})
		a.wg.Wait()
		slog.Info("agent scheduler stopped")
	})
}

// Recover re-queues messages from the DB that were interrupted by a crash.
// Must be called before Run.
func (a *Agent) Recover(ctx context.Context, frontend Frontend, spawnMedia func(msg *store.StoredMessage, readyCh chan Payload)) {
	qs, ok := a.store.(store.QueueStore)
	if !ok {
		return
	}

	recovered, notReady, err := qs.RecoverQueue(ctx)
	if err != nil {
		slog.Error("agent: recover queue", "err", err)
		return
	}
	if recovered > 0 {
		slog.Info("agent: recovered stuck messages", "count", recovered)
	}

	// notReady messages: re-spawn media processing goroutines.
	for i := range notReady {
		msg := &notReady[i]
		readyCh := make(chan Payload, 1)
		a.SubmitTask(Task{
			ChatID:       msg.ChatID,
			MsgID:        msg.ID,
			TriggerMsgID: msg.TriggerMsgID,
			Frontend:     frontend,
			ReadyCh:      readyCh,
		})
		spawnMedia(msg, readyCh)
		slog.Info("agent: re-queued notReady message", "msg_id", msg.ID, "chat_id", msg.ChatID)
	}

	// ready messages: push with trigger for drainReady.
	readyChats, err := qs.PendingChats(ctx)
	if err != nil {
		slog.Error("agent: pending chats", "err", err)
		return
	}
	for _, chatID := range readyChats {
		readyCh := make(chan Payload, 1)
		close(readyCh) // already ready — will trigger drainReady
		a.SubmitTask(Task{
			ChatID:   chatID,
			Frontend: frontend,
			ReadyCh:  readyCh,
		})
	}
}

// chatChan returns (or creates) the per-chat task channel.
func (a *Agent) chatChan(chatID string) chan Task {
	if v, ok := a.chatChans.Load(chatID); ok {
		return v.(chan Task)
	}
	ch := make(chan Task, 64)
	actual, loaded := a.chatChans.LoadOrStore(chatID, ch)
	if !loaded {
		a.wg.Add(1)
		go func() {
			defer a.wg.Done()
			a.processChat(chatID, actual.(chan Task))
		}()
	}
	return actual.(chan Task)
}

// processChat is the per-chat consumer goroutine.
func (a *Agent) processChat(chatID string, ch chan Task) {
	defer a.chatChans.Delete(chatID)

	for task := range ch {
		slog.Info("agent: popped task", "chat_id", chatID, "msg_id", task.MsgID)

		// Open thinking card IMMEDIATELY — user sees instant feedback
		// even while audio is being transcribed. If this task turns out
		// to be a duplicate (already processed by drainReady), the
		// Frontend handles the graceful close (no terminal event → dismiss card).
		eventsCh := make(chan Event, 16)
		msg := &Message{
			ChatID:       task.ChatID,
			MsgID:        task.MsgID,
			TriggerMsgID: task.TriggerMsgID,
			Lang:         task.Lang,
			Events:       eventsCh,
		}
		if task.Frontend != nil {
			task.Frontend.SubmitMessage(msg)
		}

		// Wait for media processing to complete.
		var payload Payload
		var payloadOK bool
		select {
		case p, ok := <-task.ReadyCh:
			if ok {
				payload = p
				payloadOK = true
			} else {
				// Channel closed without sending — drain trigger (crash recovery).
				payloadOK = false
			}
		case <-time.After(2 * time.Minute):
			slog.Error("agent: media processing timed out",
				"chat_id", chatID, "msg_id", task.MsgID)
			eventsCh <- Event{Type: EventError, Payload: ErrorPayload{
				Err: fmt.Errorf("media processing timed out"),
			}}
			close(eventsCh)
			// Mark as terminal so late ProcessMedia can't revert to ready.
			// SetReplyStatus has WHERE reply_status != 'done' guard.
			if qs, ok := a.store.(store.QueueStore); ok && task.MsgID > 0 {
				qs.UpdateMessageContent(a.ctx, task.MsgID, "[media processing timed out]")
				qs.FinishReply(a.ctx, task.MsgID)
			}
			a.drainReady(a.ctx, chatID, task.Frontend)
			continue
		}

		if payloadOK && task.MsgID > 0 {
			// Normal path: atomically claim THIS specific message by ID.
			// If drainReady already processed it, ClaimReplyByID returns nil → skip.
			if qs, ok := a.store.(store.QueueStore); ok {
				claimed, err := qs.ClaimReplyByID(a.ctx, chatID, task.MsgID)
				if err != nil || claimed == nil {
					slog.Info("agent: task already processed, skipping",
						"chat_id", chatID, "msg_id", task.MsgID)
					close(eventsCh) // Frontend sees close without terminal → dismisses card
					continue
				}
			}

			a.executeWithTrace(a.ctx, task, payload, eventsCh)

			if qs, ok := a.store.(store.QueueStore); ok {
				if err := qs.FinishReply(a.ctx, task.MsgID); err != nil {
					slog.Error("agent: finish reply", "msg_id", task.MsgID, "err", err)
				}
			}
		} else if payloadOK {
			// No DB tracking (e.g. cron tasks with MsgID=0).
			a.executeWithTrace(a.ctx, task, payload, eventsCh)
		} else {
			// Closed channel — drain trigger, close the card.
			close(eventsCh)
		}

		slog.Info("agent: done", "chat_id", chatID, "msg_id", task.MsgID)

		// Drain: process remaining ready messages from DB that weren't
		// delivered via channel (crash recovery, or messages that became
		// ready while the previous task was executing).
		a.drainReady(a.ctx, chatID, task.Frontend)
	}
}

// drainReady processes all remaining 'ready' messages for chatID from the DB
// without waiting on the channel. Each gets its own event stream + thinking card.
func (a *Agent) drainReady(ctx context.Context, chatID string, frontend Frontend) {
	qs, ok := a.store.(store.QueueStore)
	if !ok {
		return
	}

	for {
		claimed, err := qs.ClaimNextReply(ctx, chatID)
		if err != nil || claimed == nil {
			return
		}

		eventsCh := make(chan Event, 16)
		msg := &Message{
			ChatID:       chatID,
			MsgID:        claimed.ID,
			TriggerMsgID: claimed.TriggerMsgID,
			Lang:         detectLang(claimed.Content),
			Events:       eventsCh,
		}
		if frontend != nil {
			frontend.SubmitMessage(msg)
		}

		payload := Payload{Content: claimed.Content, FilePaths: claimed.FilePaths}
		task := Task{ChatID: chatID, MsgID: claimed.ID, TriggerMsgID: claimed.TriggerMsgID, Frontend: frontend}
		a.executeWithTrace(ctx, task, payload, eventsCh)

		if err := qs.FinishReply(ctx, claimed.ID); err != nil {
			slog.Error("agent: drain finish reply", "msg_id", claimed.ID, "err", err)
		}

		slog.Info("agent: drain-done", "chat_id", chatID, "msg_id", claimed.ID)
	}
}

// detectLang checks if the text is primarily Chinese.
func detectLang(text string) string {
	for _, r := range text {
		if r >= 0x4E00 && r <= 0x9FFF {
			return "zh"
		}
	}
	return "en"
}
