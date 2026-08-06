// Package scheduler implements the cron-based job scheduler for gh-vault.
package scheduler

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"golang.org/x/sync/errgroup"

	"github.com/RP2/gh-vault/internal/backup"
	"github.com/RP2/gh-vault/internal/model"
	"github.com/RP2/gh-vault/internal/store"
	reposync "github.com/RP2/gh-vault/internal/sync"
)

// Scheduler coordinates background cron jobs for gh-vault.
type Scheduler interface {
	Start() error
	Stop() error
	ReloadCron(expr string) error
	NextRun(jobName string) time.Time
	IsRunning(jobName string) bool
}

// CronScheduler implements Scheduler using robfig/cron/v3.
type CronScheduler struct {
	cron     *cron.Cron
	jobs     sync.Map
	running  sync.Map
	settings store.SettingsStore
	syncer   reposync.Syncer
	engine   backup.Engine
	repos    store.RepoStore
	logs     store.LogStore
	sessions store.SessionStore
}

var _ Scheduler = (*CronScheduler)(nil)

const defaultSyncSchedule = "0 3 1 * *"

// New creates a CronScheduler with the provided collaborators.
func New(
	settings store.SettingsStore,
	syncer reposync.Syncer,
	engine backup.Engine,
	repos store.RepoStore,
	logs store.LogStore,
	sessions store.SessionStore,
) *CronScheduler {
	return &CronScheduler{
		settings: settings,
		syncer:   syncer,
		engine:   engine,
		repos:    repos,
		logs:     logs,
		sessions: sessions,
	}
}

// Start loads the cron schedule from settings, registers all jobs, and starts the cron runner.
func (s *CronScheduler) Start() error {
	if s.cron != nil {
		return fmt.Errorf("scheduler: already started")
	}

	s.cron = cron.New()
	schedule := s.syncSchedule()
	if err := s.addJobs(schedule); err != nil {
		return fmt.Errorf("scheduler: start: %w", err)
	}

	s.cron.Start()
	slog.Info("scheduler: started", "sync_schedule", schedule)
	return nil
}

// Stop stops the cron runner and waits up to 30 seconds for running jobs to finish.
// Stop only stops scheduling; it does not cancel in-flight job execution.
func (s *CronScheduler) Stop() error {
	if s.cron == nil {
		return nil
	}

	ctx := s.cron.Stop()
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	<-waitCtx.Done()

	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("scheduler: stop timed out: %w", waitCtx.Err())
	}

	s.jobs.Range(func(k, v any) bool { s.jobs.Delete(k); return true })
	s.running.Range(func(k, v any) bool { s.running.Delete(k); return true })
	s.cron = nil

	slog.Info("scheduler: stopped")
	return nil
}

// ReloadCron replaces the current cron configuration with the provided sync expression.
func (s *CronScheduler) ReloadCron(expr string) error {
	if s.cron == nil {
		return errors.New("scheduler: not started")
	}
	if expr == "" {
		return errors.New("scheduler: cron expression must not be empty")
	}
	if _, err := cron.ParseStandard(expr); err != nil {
		return fmt.Errorf("scheduler: invalid cron expression: %w", err)
	}

	ctx := s.cron.Stop()
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	<-waitCtx.Done()

	if errors.Is(waitCtx.Err(), context.DeadlineExceeded) {
		return fmt.Errorf("scheduler: reload cron: stop timed out: %w", waitCtx.Err())
	}

	s.jobs.Range(func(k, v any) bool {
		s.cron.Remove(v.(cron.EntryID))
		s.jobs.Delete(k)
		return true
	})

	if err := s.addJobs(expr); err != nil {
		return fmt.Errorf("scheduler: reload cron: %w", err)
	}

	s.cron.Start()
	slog.Info("scheduler: cron reloaded", "sync_schedule", expr)
	return nil
}

// NextRun returns the next scheduled run time for a job, or the zero time if unknown.
func (s *CronScheduler) NextRun(jobName string) time.Time {
	v, ok := s.jobs.Load(jobName)
	if !ok {
		return time.Time{}
	}
	id, ok := v.(cron.EntryID)
	if !ok {
		return time.Time{}
	}
	return s.cron.Entry(id).Next
}

// IsRunning reports whether a job is currently executing.
func (s *CronScheduler) IsRunning(jobName string) bool {
	v, ok := s.running.Load(jobName)
	return ok && v == true
}

func (s *CronScheduler) syncSchedule() string {
	schedule, err := s.settings.Get("cron_schedule")
	if err != nil {
		slog.Warn("scheduler: using default sync schedule", "default", defaultSyncSchedule, "error", err)
		return defaultSyncSchedule
	}
	return schedule
}

func (s *CronScheduler) addJobs(syncExpr string) error {
	jobs := []struct {
		name string
		expr string
		fn   func(ctx context.Context)
	}{
		{name: "sync", expr: syncExpr, fn: s.syncJob},
		{name: "backup", expr: "0 2 * * *", fn: s.backupJob},
		{name: "verify", expr: "0 4 * * 0", fn: s.verifyJob},
		{name: "log_cleanup", expr: "0 5 * * *", fn: s.logCleanupJob},
		{name: "session_cleanup", expr: "5 5 * * *", fn: s.sessionCleanupJob},
	}

	for _, j := range jobs {
		id, err := s.cron.AddFunc(j.expr, s.wrapJob(j.name, j.fn))
		if err != nil {
			return fmt.Errorf("scheduler: add job %q: %w", j.name, err)
		}
		s.jobs.Store(j.name, id)
	}
	return nil
}

func (s *CronScheduler) wrapJob(name string, fn func(ctx context.Context)) func() {
	return func() {
		if _, loaded := s.running.LoadOrStore(name, true); loaded {
			slog.Warn("scheduler: job already running, skipping", "job", name)
			return
		}
		defer s.running.Delete(name)

		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
		defer cancel()

		fn(ctx)
	}
}

func (s *CronScheduler) syncJob(ctx context.Context) {
	result, err := s.syncer.SyncRepos(ctx)
	if err != nil {
		slog.Error("scheduler: sync job failed", "error", err)
		return
	}
	slog.Info("scheduler: sync job completed",
		"added", result.Added,
		"updated", result.Updated,
		"renamed", result.Renamed,
		"transferred", result.Transferred,
		"deleted", result.Deleted,
		"unchanged", result.Unchanged,
		"errors", result.ErrorCount,
	)
}

func (s *CronScheduler) backupJob(ctx context.Context) {
	repos, err := s.repos.List()
	if err != nil {
		slog.Error("scheduler: backup job: list repos", "error", err)
		return
	}

	var active []model.Repo
	for _, r := range repos {
		if !r.GitHubDeleted && r.BackupEnabled {
			active = append(active, r)
		}
	}

	var (
		failed int
		mu     sync.Mutex
	)
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(3)
	for _, r := range active {
		r := r
		g.Go(func() error {
			if r.Format == model.FormatBundle {
				if err := s.engine.CloneRepo(ctx, r); err != nil {
					slog.Error("scheduler: backup job: clone repo", "owner", r.Owner, "name", r.Name, "error", err)
					mu.Lock()
					failed++
					mu.Unlock()
					return err
				}
				if err := s.engine.CreateBundle(ctx, r); err != nil {
					slog.Error("scheduler: backup job: create bundle", "owner", r.Owner, "name", r.Name, "error", err)
					mu.Lock()
					failed++
					mu.Unlock()
					return err
				}
			} else {
				if err := s.engine.CloneRepo(ctx, r); err != nil {
					slog.Error("scheduler: backup job: clone repo", "owner", r.Owner, "name", r.Name, "error", err)
					mu.Lock()
					failed++
					mu.Unlock()
					return err
				}
			}
			now := time.Now()
			if err := s.repos.SetLastBackup(r.ID, &now); err != nil {
				slog.Error("cron backup: set last_backup", "repo", r.Name, "error", err)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		slog.Error("scheduler: backup job failed", "error", err)
	}
	slog.Info("scheduler: backup job completed", "repos", len(active), "failed", failed)
}

func (s *CronScheduler) verifyJob(ctx context.Context) {
	repos, err := s.repos.List()
	if err != nil {
		slog.Error("scheduler: verify job: list repos", "error", err)
		return
	}

	var failed int
	for _, r := range repos {
		if r.GitHubDeleted || r.BackupPath == nil {
			continue
		}
		if err := s.engine.Verify(ctx, r); err != nil {
			slog.Error("scheduler: verify job: verify repo", "owner", r.Owner, "name", r.Name, "error", err)
			failed++
		}
	}
	slog.Info("scheduler: verify job completed", "repos", len(repos), "failed", failed)
}

func (s *CronScheduler) logCleanupJob(ctx context.Context) {
	daysStr, err := s.settings.Get("log_retention_days")
	if err != nil {
		slog.Error("scheduler: log cleanup job: get retention days", "error", err)
		return
	}

	days, err := strconv.Atoi(daysStr)
	if err != nil {
		slog.Error("scheduler: log cleanup job: parse retention days", "value", daysStr, "error", err)
		return
	}
	if days <= 0 {
		slog.Info("scheduler: log cleanup job disabled", "retention_days", days)
		return
	}

	cutoff := time.Now().AddDate(0, 0, -days)
	deleted, err := s.logs.DeleteOlderThan(cutoff)
	if err != nil {
		slog.Error("scheduler: log cleanup job: delete old logs", "error", err)
		return
	}
	slog.Info("scheduler: log cleanup job completed", "deleted", deleted, "cutoff", cutoff)
}

func (s *CronScheduler) sessionCleanupJob(ctx context.Context) {
	deleted, err := s.sessions.DeleteExpired()
	if err != nil {
		slog.Error("scheduler: session cleanup job failed", "error", err)
		return
	}
	slog.Info("scheduler: session cleanup job completed", "deleted", deleted)
}
