package store

import (
	"context"
	"testing"
	"time"
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

func TestMemoryCronStore(t *testing.T) {
	ctx := context.Background()
	st := NewMemoryStore()
	now := time.Now()
	next := now.Add(-time.Minute)

	if err := st.CreateCronSchedule(ctx, &CronSchedule{
		ChatID:    "chat",
		Name:      "daily",
		Hour:      9,
		Minute:    30,
		Timezone:  "UTC",
		Prompt:    "Run report",
		NextRunAt: &next,
	}); err != nil {
		t.Fatalf("CreateCronSchedule: %v", err)
	}

	due, err := st.DueCronSchedules(ctx, now, 10)
	if err != nil {
		t.Fatalf("DueCronSchedules: %v", err)
	}
	if len(due) != 1 || due[0].Name != "daily" {
		t.Fatalf("due = %#v, want one daily schedule", due)
	}

	later := now.Add(24 * time.Hour)
	if err := st.MarkCronScheduleRun(ctx, due[0].ID, now, later); err != nil {
		t.Fatalf("MarkCronScheduleRun: %v", err)
	}
	due, err = st.DueCronSchedules(ctx, now, 10)
	if err != nil {
		t.Fatalf("DueCronSchedules after mark: %v", err)
	}
	if len(due) != 0 {
		t.Fatalf("due after mark = %#v, want none", due)
	}

	deleted, err := st.DeleteCronSchedule(ctx, 1, "chat")
	if err != nil {
		t.Fatalf("DeleteCronSchedule: %v", err)
	}
	if !deleted {
		t.Fatal("expected schedule to be disabled")
	}
	list, err := st.ListCronSchedules(ctx, "chat", false)
	if err != nil {
		t.Fatalf("ListCronSchedules: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("enabled schedules = %#v, want none", list)
	}
}
