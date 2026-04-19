package cron

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"argus/internal/store"
)

// DBScheduler polls persisted cron schedules and turns due schedules into
// async tasks. It does not run the Agent directly.
type DBScheduler struct {
	cronStore store.CronStore
	taskStore store.TaskStore
	interval  time.Duration
	batchSize int
	closeCh   chan struct{}
	wg        sync.WaitGroup
}

func NewDBScheduler(cronStore store.CronStore, taskStore store.TaskStore, interval time.Duration) *DBScheduler {
	if interval == 0 {
		interval = time.Minute
	}
	return &DBScheduler{
		cronStore: cronStore,
		taskStore: taskStore,
		interval:  interval,
		batchSize: 20,
		closeCh:   make(chan struct{}),
	}
}

func (s *DBScheduler) Start() {
	s.wg.Add(1)
	go s.run()
	slog.Info("db cron scheduler started", "interval", s.interval)
}

func (s *DBScheduler) Stop() {
	close(s.closeCh)
	s.wg.Wait()
	slog.Info("db cron scheduler stopped")
}

func (s *DBScheduler) run() {
	defer s.wg.Done()

	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			s.processDue(context.Background(), time.Now())
		case <-s.closeCh:
			return
		}
	}
}

func (s *DBScheduler) processDue(ctx context.Context, now time.Time) {
	due, err := s.cronStore.DueCronSchedules(ctx, now, s.batchSize)
	if err != nil {
		slog.Warn("fetch due cron schedules", "err", err)
		return
	}
	for _, schedule := range due {
		if err := s.emitTask(ctx, schedule, now); err != nil {
			slog.Warn("emit cron task", "schedule_id", schedule.ID, "err", err)
			continue
		}
		next, err := NextDailyRun(now, schedule.Hour, schedule.Minute, schedule.Timezone)
		if err != nil {
			slog.Warn("compute next cron run", "schedule_id", schedule.ID, "err", err)
			continue
		}
		if err := s.cronStore.MarkCronScheduleRun(ctx, schedule.ID, now, next); err != nil {
			slog.Warn("mark cron schedule run", "schedule_id", schedule.ID, "err", err)
		}
	}
}

func (s *DBScheduler) emitTask(ctx context.Context, schedule store.CronSchedule, now time.Time) error {
	input, err := json.Marshal(map[string]string{
		"prompt": schedule.Prompt,
	})
	if err != nil {
		return fmt.Errorf("marshal task input: %w", err)
	}
	task := &store.Task{
		Kind:     "async",
		Source:   "cron",
		ChatID:   schedule.ChatID,
		UserID:   schedule.UserID,
		Status:   "queued",
		Title:    schedule.Name,
		Input:    input,
		Priority: 0,
	}
	if err := s.taskStore.CreateTask(ctx, task); err != nil {
		return err
	}
	slog.Info("cron emitted async task",
		"schedule_id", schedule.ID,
		"task_id", task.ID,
		"next_from", now,
	)
	return nil
}

// NextDailyRun returns the next occurrence of hour:minute in the given timezone
// strictly after base.
func NextDailyRun(base time.Time, hour, minute int, timezone string) (time.Time, error) {
	if hour < 0 || hour > 23 {
		return time.Time{}, fmt.Errorf("hour out of range: %d", hour)
	}
	if minute < 0 || minute > 59 {
		return time.Time{}, fmt.Errorf("minute out of range: %d", minute)
	}
	if timezone == "" {
		timezone = "Asia/Shanghai"
	}
	loc, err := time.LoadLocation(timezone)
	if err != nil {
		return time.Time{}, fmt.Errorf("load timezone: %w", err)
	}
	local := base.In(loc)
	next := time.Date(local.Year(), local.Month(), local.Day(), hour, minute, 0, 0, loc)
	if !next.After(local) {
		next = next.AddDate(0, 0, 1)
	}
	return next, nil
}
