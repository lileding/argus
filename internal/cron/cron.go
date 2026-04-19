package cron

import (
	"log/slog"
	"sync"
	"time"
)

// Job is a scheduled task.
type Job struct {
	Name     string
	Schedule string // cron-like: "HH:MM" for daily
	Fn       func()
	hour     int
	minute   int
}

// Scheduler runs jobs on a daily schedule.
// MVP uses simple HH:MM daily scheduling, not full cron expressions.
type Scheduler struct {
	jobs    []*Job
	closeCh chan struct{}
	wg      sync.WaitGroup
}

func NewScheduler() *Scheduler {
	return &Scheduler{
		closeCh: make(chan struct{}),
	}
}

// AddDaily adds a job that runs daily at the specified hour and minute.
func (s *Scheduler) AddDaily(name string, hour, minute int, fn func()) {
	s.jobs = append(s.jobs, &Job{
		Name:   name,
		hour:   hour,
		minute: minute,
		Fn:     fn,
	})
}

// Start begins the scheduler loop. Call Stop() to shut down.
func (s *Scheduler) Start() {
	s.wg.Add(1)
	go s.run()
	slog.Info("cron scheduler started", "jobs", len(s.jobs))
}

func (s *Scheduler) run() {
	defer s.wg.Done()

	// Check every minute.
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			now := time.Now()
			for _, job := range s.jobs {
				if now.Hour() == job.hour && now.Minute() == job.minute {
					slog.Info("cron job triggered", "job", job.Name)
					go s.runJob(job)
				}
			}
		case <-s.closeCh:
			return
		}
	}
}

func (s *Scheduler) runJob(job *Job) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("cron job panicked", "job", job.Name, "panic", r)
		}
	}()
	job.Fn()
}

func (s *Scheduler) Stop() {
	close(s.closeCh)
	s.wg.Wait()
	slog.Info("cron scheduler stopped")
}
