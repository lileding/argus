package feishu

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"argus/internal/agent"
	"argus/internal/store"
)

// Dispatcher serializes agent processing per chat. Different chats run in
// parallel; within a single chat, messages are processed one at a time in
// FIFO order. The messages table (via QueueStore) is the source of truth;
// an in-memory channel + periodic scan provides low-latency notification.
type Dispatcher struct {
	store   store.QueueStore
	agent   *agent.Agent
	adapter *Adapter

	active sync.Map      // chatID → struct{}: at most one goroutine per chat
	notify <-chan string // chatID notification from upstream (Filter)
	quit   chan struct{}
	wg     sync.WaitGroup
}

// NewDispatcher creates a Dispatcher. notify is the channel that upstream
// (Filter) sends chatID notifications on when a message becomes 'ready'.
func NewDispatcher(st store.QueueStore, ag *agent.Agent, adapter *Adapter, notify <-chan string) *Dispatcher {
	return &Dispatcher{
		store:   st,
		agent:   ag,
		adapter: adapter,
		notify:  notify,
		quit:    make(chan struct{}),
	}
}

// Start launches the dispatcher loop and runs crash recovery.
func (d *Dispatcher) Start(ctx context.Context) {
	// Crash recovery: reset stuck rows, re-notify pending chats.
	recovered, unacked, err := d.store.RecoverQueue(ctx)
	if err != nil {
		slog.Error("dispatcher: recover queue", "err", err)
	} else {
		if recovered > 0 {
			slog.Info("dispatcher: recovered stuck messages", "count", recovered)
		}
		if len(unacked) > 0 {
			slog.Warn("dispatcher: unacked messages found (need Filter re-processing)", "count", len(unacked))
			// Notify Filter for these — but we don't own Filter's channel.
			// Caller (main.go) should handle unacked separately.
		}
	}

	// Directly process any 'ready' messages left from before the crash.
	readyChats, err := d.store.PendingChats(ctx)
	if err != nil {
		slog.Error("dispatcher: pending chats", "err", err)
	}
	for _, chatID := range readyChats {
		d.tryProcess(chatID)
	}

	d.wg.Add(1)
	go d.run()
	slog.Info("dispatcher started")
}

func (d *Dispatcher) Stop() {
	close(d.quit)
	d.wg.Wait()
	slog.Info("dispatcher stopped")
}

// Notification comes from the upstream channel (Filter writes to it).
// No Notify method needed — the dispatcher reads from the channel directly.

func (d *Dispatcher) run() {
	defer d.wg.Done()
	ticker := time.NewTicker(5 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case chatID := <-d.notify:
			d.tryProcess(chatID)
		case <-ticker.C:
			d.scanReady()
		case <-d.quit:
			return
		}
	}
}

func (d *Dispatcher) tryProcess(chatID string) {
	if _, loaded := d.active.LoadOrStore(chatID, struct{}{}); loaded {
		return // already processing this chat
	}
	d.wg.Add(1)
	go func() {
		defer d.wg.Done()
		d.processChat(chatID)
	}()
}

func (d *Dispatcher) processChat(chatID string) {
	defer d.active.Delete(chatID)

	ctx := context.Background()
	for {
		msg, err := d.store.ClaimNextReply(ctx, chatID)
		if err != nil {
			slog.Error("dispatcher: claim next", "chat_id", chatID, "err", err)
			return
		}
		if msg == nil {
			return // no more work for this chat
		}

		slog.Info("dispatcher: processing message",
			"chat_id", chatID,
			"msg_id", msg.ID,
			"content_preview", truncateForLog(msg.Content, 60),
		)

		ch := d.agent.HandleStreamQueued(ctx, chatID, msg.ID, msg.Content)
		d.adapter.HandleEvents(ch, msg.TriggerMsgID, msg.ReplyChannelID, msg.Content)

		if err := d.store.FinishReply(ctx, msg.ID); err != nil {
			slog.Error("dispatcher: finish reply", "msg_id", msg.ID, "err", err)
		}

		slog.Info("dispatcher: message done", "chat_id", chatID, "msg_id", msg.ID)
	}
}

func (d *Dispatcher) scanReady() {
	ctx := context.Background()
	chats, err := d.store.PendingChats(ctx)
	if err != nil {
		slog.Debug("dispatcher: scan ready", "err", err)
		return
	}
	for _, chatID := range chats {
		d.tryProcess(chatID)
	}
}

func truncateForLog(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
