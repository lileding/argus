package store

import (
	"context"
	"testing"
)

func TestMemoryOutboxClaimAndRecover(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()

	if err := st.CreateOutboxEvent(ctx, &OutboxEvent{
		ChatID:   "chat",
		Kind:     "async_done",
		Payload:  []byte(`{"title":"high"}`),
		Priority: 10,
	}); err != nil {
		t.Fatalf("CreateOutboxEvent high: %v", err)
	}
	if err := st.CreateOutboxEvent(ctx, &OutboxEvent{
		ChatID:   "chat",
		Kind:     "async_done",
		Payload:  []byte(`{"title":"low"}`),
		Priority: 0,
	}); err != nil {
		t.Fatalf("CreateOutboxEvent low: %v", err)
	}

	chats, err := st.PendingOutboxChats(ctx)
	if err != nil {
		t.Fatalf("PendingOutboxChats: %v", err)
	}
	if len(chats) != 1 || chats[0] != "chat" {
		t.Fatalf("chats = %#v, want [chat]", chats)
	}

	event, err := st.ClaimNextOutboxEvent(ctx, "chat")
	if err != nil {
		t.Fatalf("ClaimNextOutboxEvent: %v", err)
	}
	if event == nil || event.Priority != 10 {
		t.Fatalf("claimed event = %#v, want priority 10", event)
	}

	recovered, err := st.RecoverOutbox(ctx)
	if err != nil {
		t.Fatalf("RecoverOutbox: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}

	event, err = st.ClaimNextOutboxEvent(ctx, "chat")
	if err != nil {
		t.Fatalf("ClaimNextOutboxEvent after recover: %v", err)
	}
	if event == nil || event.Priority != 10 {
		t.Fatalf("claimed event after recover = %#v, want priority 10", event)
	}
	if err := st.MarkOutboxSent(ctx, event.ID); err != nil {
		t.Fatalf("MarkOutboxSent: %v", err)
	}

	event, err = st.ClaimNextOutboxEvent(ctx, "chat")
	if err != nil {
		t.Fatalf("ClaimNextOutboxEvent low: %v", err)
	}
	if event == nil || event.Priority != 0 {
		t.Fatalf("claimed event = %#v, want priority 0", event)
	}
}
