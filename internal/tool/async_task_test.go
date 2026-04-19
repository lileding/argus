package tool

import (
	"context"
	"strings"
	"testing"
	"time"

	"argus/internal/store"
)

func TestCreateAsyncTaskToolCreatesQueuedTask(t *testing.T) {
	st := store.NewMemoryStore()
	tool := NewCreateAsyncTaskTool(st)
	ctx := WithChatID(context.Background(), "p2p:test")

	out, err := tool.Execute(ctx, `{"title":"Run tests","prompt":"Run the test suite and report failures.","priority":3}`)
	if err != nil {
		t.Fatalf("Execute returned error: %v", err)
	}
	if !strings.Contains(out, "Created async task 1") {
		t.Fatalf("unexpected output: %s", out)
	}

	task, err := st.GetTask(ctx, 1)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task == nil {
		t.Fatal("expected task to be created")
	}
	if task.ChatID != "p2p:test" {
		t.Fatalf("ChatID = %q, want p2p:test", task.ChatID)
	}
	if task.Status != "queued" {
		t.Fatalf("Status = %q, want queued", task.Status)
	}
	if task.Priority != 3 {
		t.Fatalf("Priority = %d, want 3", task.Priority)
	}
	if !strings.Contains(string(task.Input), "Run the test suite") {
		t.Fatalf("Input = %s", task.Input)
	}
}

func TestTaskStatusAndCancelTools(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()
	task := &store.Task{
		Kind:   "async",
		Source: "agent",
		ChatID: "p2p:test",
		Title:  "Long job",
		Input:  []byte(`{"prompt":"work"}`),
	}
	if err := st.CreateTask(ctx, task); err != nil {
		t.Fatalf("CreateTask returned error: %v", err)
	}

	statusTool := NewGetTaskStatusTool(st)
	out, err := statusTool.Execute(ctx, `{"id":"1"}`)
	if err != nil {
		t.Fatalf("status Execute returned error: %v", err)
	}
	if !strings.Contains(out, "Status: queued") {
		t.Fatalf("unexpected status output: %s", out)
	}

	cancelTool := NewCancelTaskTool(st)
	out, err = cancelTool.Execute(ctx, `{"id":"1"}`)
	if err != nil {
		t.Fatalf("cancel Execute returned error: %v", err)
	}
	if out != "Task 1 cancelled." {
		t.Fatalf("unexpected cancel output: %s", out)
	}

	task, err = st.GetTask(ctx, 1)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.Status != "cancelled" {
		t.Fatalf("Status = %q, want cancelled", task.Status)
	}
}

func TestMemoryTaskClaimPriorityAndRecoverExpired(t *testing.T) {
	ctx := context.Background()
	st := store.NewMemoryStore()

	if err := st.CreateTask(ctx, &store.Task{Title: "low", Priority: 0, ChatID: "chat"}); err != nil {
		t.Fatalf("CreateTask low: %v", err)
	}
	if err := st.CreateTask(ctx, &store.Task{Title: "high", Priority: 10, ChatID: "chat"}); err != nil {
		t.Fatalf("CreateTask high: %v", err)
	}

	leaseUntil := time.Now().Add(-time.Minute)
	task, err := st.ClaimNextTask(ctx, "worker-1", leaseUntil)
	if err != nil {
		t.Fatalf("ClaimNextTask returned error: %v", err)
	}
	if task == nil || task.Title != "high" {
		t.Fatalf("claimed task = %#v, want high priority task", task)
	}

	recovered, err := st.RecoverExpiredTasks(ctx, time.Now())
	if err != nil {
		t.Fatalf("RecoverExpiredTasks returned error: %v", err)
	}
	if recovered != 1 {
		t.Fatalf("recovered = %d, want 1", recovered)
	}

	task, err = st.GetTask(ctx, task.ID)
	if err != nil {
		t.Fatalf("GetTask returned error: %v", err)
	}
	if task.Status != "queued" {
		t.Fatalf("Status = %q, want queued", task.Status)
	}
}
