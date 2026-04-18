package feishu

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"argus/internal/agent"
	"argus/internal/store"
)

// QueuedMessage is pushed into a per-chat channel by the Handler.
// ReadyCh is closed by the media-processing goroutine once the message
// content in the DB has been updated to its final processed form.
type QueuedMessage struct {
	MsgID        int64
	ChatID       string
	TriggerMsgID string
	Lang         string        // pre-detected language ("zh"/"en") for thinking card
	ReadyCh      chan struct{} // closed when media processing is done
}

// Dispatcher serializes agent processing per chat. Each chat gets a
// buffered channel (MPSC: Handler pushes, one goroutine consumes).
// When a message is popped, the Dispatcher opens the thinking card
// immediately, then blocks on ReadyCh until the content is ready.
type Dispatcher struct {
	store   store.QueueStore
	agent   *agent.Agent
	adapter *Adapter
	client  *Client

	// Per-chat message channels. Created lazily on first push.
	// Value type: chan QueuedMessage
	chatChans sync.Map

	wg sync.WaitGroup
}

func NewDispatcher(st store.QueueStore, ag *agent.Agent, adapter *Adapter, client *Client) *Dispatcher {
	return &Dispatcher{
		store:   st,
		agent:   ag,
		adapter: adapter,
		client:  client,
	}
}

// ChatChan returns (or creates) the per-chat message channel.
// If this is a new chat, it also starts the consumer goroutine.
func (d *Dispatcher) ChatChan(chatID string) chan QueuedMessage {
	if v, ok := d.chatChans.Load(chatID); ok {
		return v.(chan QueuedMessage)
	}
	ch := make(chan QueuedMessage, 64)
	actual, loaded := d.chatChans.LoadOrStore(chatID, ch)
	if !loaded {
		// First message for this chat — start consumer.
		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			d.processChat(chatID, actual.(chan QueuedMessage))
		}()
	}
	return actual.(chan QueuedMessage)
}

// Recover re-queues messages from the DB that were interrupted by a crash.
// Must be called before the Handler starts accepting new messages.
func (d *Dispatcher) Recover(ctx context.Context, spawnMediaProcessor func(msg *store.StoredMessage, readyCh chan struct{})) {
	recovered, notReady, err := d.store.RecoverQueue(ctx)
	if err != nil {
		slog.Error("dispatcher: recover queue", "err", err)
		return
	}
	if recovered > 0 {
		slog.Info("dispatcher: recovered stuck messages", "count", recovered)
	}

	// notReady messages: re-spawn media processing goroutines.
	for i := range notReady {
		msg := &notReady[i]
		readyCh := make(chan struct{})
		d.ChatChan(msg.ChatID) <- QueuedMessage{
			MsgID:        msg.ID,
			ChatID:       msg.ChatID,
			TriggerMsgID: msg.TriggerMsgID,
			ReadyCh:      readyCh,
		}
		spawnMediaProcessor(msg, readyCh)
		slog.Info("dispatcher: re-queued notReady message", "msg_id", msg.ID, "chat_id", msg.ChatID)
	}

	// ready messages: push with already-closed readyCh.
	readyChats, err := d.store.PendingChats(ctx)
	if err != nil {
		slog.Error("dispatcher: pending chats", "err", err)
		return
	}
	for _, chatID := range readyChats {
		d.requeueReady(ctx, chatID)
	}
}

func (d *Dispatcher) requeueReady(ctx context.Context, chatID string) {
	// Peek at ready messages for this chat and push them with closed channels.
	for {
		// ClaimNextReply will mark as processing — but we want to process them
		// through the normal path. Instead, just push placeholder entries.
		// The processChat goroutine will re-claim from DB.
		//
		// Simpler: push a single trigger. processChat loops until DB is empty.
		readyCh := make(chan struct{})
		close(readyCh) // already ready
		d.ChatChan(chatID) <- QueuedMessage{
			ChatID:  chatID,
			ReadyCh: readyCh,
		}
		return // one trigger is enough — processChat drains the DB
	}
}

func (d *Dispatcher) Stop() {
	// Close all chat channels to unblock consumer goroutines.
	d.chatChans.Range(func(key, value any) bool {
		close(value.(chan QueuedMessage))
		return true
	})
	d.wg.Wait()
	slog.Info("dispatcher stopped")
}

func (d *Dispatcher) processChat(chatID string, ch chan QueuedMessage) {
	defer d.chatChans.Delete(chatID)

	ctx := context.Background()
	for msg := range ch {
		slog.Info("dispatcher: popped message",
			"chat_id", chatID, "msg_id", msg.MsgID,
		)

		// Open thinking card immediately — user sees instant feedback.
		replyChannelID := ""
		if msg.TriggerMsgID != "" {
			lang := msg.Lang
			if lang == "" {
				lang = "zh"
			}
			cardJSON := ThinkingCard(lang)
			if id, err := d.client.ReplyRichWithID(msg.TriggerMsgID, "interactive", cardJSON); err != nil {
				slog.Warn("dispatcher: send thinking card", "msg_id", msg.MsgID, "err", err)
			} else {
				replyChannelID = id
			}
		}

		// Wait for media processing to complete (with safety timeout).
		select {
		case <-msg.ReadyCh:
			// normal path
		case <-time.After(2 * time.Minute):
			slog.Error("dispatcher: media processing timed out, skipping",
				"chat_id", chatID, "msg_id", msg.MsgID)
			if replyChannelID != "" {
				d.client.UpdateMessage(replyChannelID, MarkdownToCard("⚠️ Message processing timed out."))
			}
			continue
		}

		// Reload processed content from DB.
		claimed, err := d.store.ClaimNextReply(ctx, chatID)
		if err != nil {
			slog.Error("dispatcher: claim", "chat_id", chatID, "err", err)
			continue
		}
		if claimed == nil {
			slog.Warn("dispatcher: nothing to claim after readyCh", "chat_id", chatID)
			continue
		}

		// Store reply_channel_id.
		if replyChannelID != "" {
			d.store.AckReply(ctx, claimed.ID, replyChannelID)
		}

		slog.Info("dispatcher: processing",
			"chat_id", chatID,
			"msg_id", claimed.ID,
			"content_preview", truncateForLog(claimed.Content, 60),
		)

		agentCh := d.agent.HandleStreamQueued(ctx, chatID, claimed.ID, claimed.Content)
		d.adapter.HandleEvents(agentCh, claimed.TriggerMsgID, replyChannelID, claimed.Content)

		if err := d.store.FinishReply(ctx, claimed.ID); err != nil {
			slog.Error("dispatcher: finish reply", "msg_id", claimed.ID, "err", err)
		}

		slog.Info("dispatcher: done", "chat_id", chatID, "msg_id", claimed.ID)

		// Drain: try to claim more ready messages from DB before blocking
		// on the channel. This handles the crash-recovery case where
		// requeueReady pushed only one trigger but multiple messages are
		// ready in the DB.
		d.drainReady(ctx, chatID)
	}
}

// drainReady processes all remaining 'ready' messages for chatID without
// waiting on the channel. Each gets its own thinking card since ReadyCh
// was already closed (pre-processed content in DB).
func (d *Dispatcher) drainReady(ctx context.Context, chatID string) {
	for {
		claimed, err := d.store.ClaimNextReply(ctx, chatID)
		if err != nil || claimed == nil {
			return
		}

		slog.Info("dispatcher: drain-processing",
			"chat_id", chatID, "msg_id", claimed.ID,
			"content_preview", truncateForLog(claimed.Content, 60),
		)

		replyChannelID := ""
		if claimed.TriggerMsgID != "" {
			lang := quickDetectLang(claimed.Content)
			cardJSON := ThinkingCard(lang)
			if id, err := d.client.ReplyRichWithID(claimed.TriggerMsgID, "interactive", cardJSON); err == nil {
				replyChannelID = id
				d.store.AckReply(ctx, claimed.ID, replyChannelID)
			}
		}

		agentCh := d.agent.HandleStreamQueued(ctx, chatID, claimed.ID, claimed.Content)
		d.adapter.HandleEvents(agentCh, claimed.TriggerMsgID, replyChannelID, claimed.Content)

		if err := d.store.FinishReply(ctx, claimed.ID); err != nil {
			slog.Error("dispatcher: drain finish", "msg_id", claimed.ID, "err", err)
		}
		slog.Info("dispatcher: drain-done", "chat_id", chatID, "msg_id", claimed.ID)
	}
}

func truncateForLog(s string, max int) string {
	r := []rune(s)
	if len(r) <= max {
		return s
	}
	return string(r[:max]) + "..."
}
