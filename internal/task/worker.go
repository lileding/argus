package task

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"argus/internal/agent"
	"argus/internal/model"
	"argus/internal/store"
)

type Worker struct {
	store         store.TaskStore
	agent         *agent.Agent
	workerID      string
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
		w.fail(ctx, task.ID, err)
		return
	}

	msg := model.NewTextMessage(model.RoleUser, prompt)
	msg.Meta = &model.MessageMeta{
		SourceIM: "task",
		Channel:  task.ChatID,
		MsgType:  "text",
	}
	reply, err := w.agent.Handle(ctx, task.ChatID, msg)
	if err != nil {
		w.fail(ctx, task.ID, err)
		return
	}
	if err := w.store.CompleteTask(ctx, task.ID, reply); err != nil {
		slog.Warn("complete async task", "task_id", task.ID, "err", err)
		return
	}

	slog.Info("async task succeeded", "task_id", task.ID)
}

func (w *Worker) fail(ctx context.Context, taskID int64, err error) {
	slog.Warn("async task failed", "task_id", taskID, "err", err)
	if failErr := w.store.FailTask(ctx, taskID, err.Error()); failErr != nil {
		slog.Warn("mark async task failed", "task_id", taskID, "err", failErr)
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
