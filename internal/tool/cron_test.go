package tool

import (
	"context"
	"strings"
	"testing"

	"argus/internal/store"
)

func TestCronToolsCreateListDelete(t *testing.T) {
	ctx := WithChatID(context.Background(), "p2p:test")
	st := store.NewMemoryStore()

	create := NewCreateCronTool(st)
	out, err := create.Execute(ctx, `{"name":"daily food","hour":22,"minute":0,"timezone":"UTC","prompt":"Summarize food."}`)
	if err != nil {
		t.Fatalf("create cron returned error: %v", err)
	}
	if !strings.Contains(out, "Created daily schedule 1") {
		t.Fatalf("unexpected create output: %s", out)
	}

	list := NewListCronTool(st)
	out, err = list.Execute(ctx, `{}`)
	if err != nil {
		t.Fatalf("list cron returned error: %v", err)
	}
	if !strings.Contains(out, "daily food") || !strings.Contains(out, "22:00 UTC") {
		t.Fatalf("unexpected list output: %s", out)
	}

	del := NewDeleteCronTool(st)
	out, err = del.Execute(ctx, `{"id":"1"}`)
	if err != nil {
		t.Fatalf("delete cron returned error: %v", err)
	}
	if out != "Schedule 1 disabled." {
		t.Fatalf("unexpected delete output: %s", out)
	}

	out, err = list.Execute(ctx, `{}`)
	if err != nil {
		t.Fatalf("list after delete returned error: %v", err)
	}
	if out != "No schedules for this chat." {
		t.Fatalf("unexpected list after delete output: %s", out)
	}
}
