package feishu

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"argus/internal/store"
)

// OutboxPresenter delivers deferred outbox events to Feishu.
// It only sends when the per-chat PresentationLock is free.
type OutboxPresenter struct {
	store     store.OutboxStore
	client    *Client
	processor MarkdownProcessor
	lock      *PresentationLock
	interval  time.Duration
	quit      chan struct{}
	wg        sync.WaitGroup
}

func NewOutboxPresenter(st store.OutboxStore, client *Client, processor MarkdownProcessor, lock *PresentationLock, interval time.Duration) *OutboxPresenter {
	if interval == 0 {
		interval = 2 * time.Second
	}
	return &OutboxPresenter{
		store:     st,
		client:    client,
		processor: processor,
		lock:      lock,
		interval:  interval,
		quit:      make(chan struct{}),
	}
}

func (p *OutboxPresenter) Start() {
	p.wg.Add(1)
	go p.run()
	slog.Info("outbox presenter started", "interval", p.interval)
}

func (p *OutboxPresenter) Stop() {
	close(p.quit)
	p.wg.Wait()
	slog.Info("outbox presenter stopped")
}

func (p *OutboxPresenter) run() {
	defer p.wg.Done()

	ticker := time.NewTicker(p.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			p.processPending()
		case <-p.quit:
			return
		}
	}
}

func (p *OutboxPresenter) processPending() {
	ctx := context.Background()
	chats, err := p.store.PendingOutboxChats(ctx)
	if err != nil {
		slog.Warn("list pending outbox chats", "err", err)
		return
	}
	for _, chatID := range chats {
		if p.lock != nil && !p.lock.TryBegin(chatID) {
			continue
		}
		p.processChat(ctx, chatID)
		if p.lock != nil {
			p.lock.End(chatID)
		}
	}
}

func (p *OutboxPresenter) processChat(ctx context.Context, chatID string) {
	for {
		event, err := p.store.ClaimNextOutboxEvent(ctx, chatID)
		if err != nil {
			slog.Warn("claim outbox event", "chat_id", chatID, "err", err)
			return
		}
		if event == nil {
			return
		}
		if err := p.deliver(event); err != nil {
			slog.Warn("deliver outbox event", "event_id", event.ID, "chat_id", event.ChatID, "err", err)
			p.store.MarkOutboxError(ctx, event.ID, err.Error())
			continue
		}
		if err := p.store.MarkOutboxSent(ctx, event.ID); err != nil {
			slog.Warn("mark outbox sent", "event_id", event.ID, "err", err)
		}
	}
}

func (p *OutboxPresenter) deliver(event *store.OutboxEvent) error {
	receiveIDType, receiveID := ParseChatID(event.ChatID)
	if receiveID == "" {
		return fmt.Errorf("cannot derive Feishu receiver from chat_id %q", event.ChatID)
	}
	md, err := outboxMarkdown(event)
	if err != nil {
		return err
	}
	if p.processor != nil {
		md = p.processor.ProcessMarkdown(md)
	}
	cardJSON := MarkdownToCard(md)
	return p.client.SendMessageRich(receiveIDType, receiveID, "interactive", cardJSON)
}

func outboxMarkdown(event *store.OutboxEvent) (string, error) {
	var payload struct {
		Title  string `json:"title"`
		Result string `json:"result"`
		Error  string `json:"error"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return "", fmt.Errorf("parse outbox payload: %w", err)
	}
	title := strings.TrimSpace(payload.Title)
	if title == "" {
		title = fmt.Sprintf("Task %d", event.ID)
	}

	switch event.Kind {
	case "async_done":
		result := strings.TrimSpace(payload.Result)
		if result == "" {
			result = "(no result)"
		}
		return fmt.Sprintf("### 后台任务完成：%s\n\n%s", title, result), nil
	case "async_failed":
		errText := strings.TrimSpace(payload.Error)
		if errText == "" {
			errText = "unknown error"
		}
		return fmt.Sprintf("### 后台任务失败：%s\n\n%s", title, errText), nil
	default:
		result := strings.TrimSpace(payload.Result)
		if result == "" {
			result = strings.TrimSpace(payload.Error)
		}
		if result == "" {
			result = string(event.Payload)
		}
		return fmt.Sprintf("### %s\n\n%s", title, result), nil
	}
}

func ParseChatID(chatID string) (receiveIDType, receiveID string) {
	if strings.HasPrefix(chatID, "p2p:") {
		return "open_id", strings.TrimPrefix(chatID, "p2p:")
	}
	if strings.HasPrefix(chatID, "group:") {
		return "chat_id", strings.TrimPrefix(chatID, "group:")
	}
	return "chat_id", chatID
}
