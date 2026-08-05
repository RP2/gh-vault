package workflow

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/RP2/gh-vault/internal/github"
	"github.com/RP2/gh-vault/internal/model"
	"github.com/RP2/gh-vault/internal/store"
)

type ArchiveWorkflow interface {
	RunEligible(ctx context.Context) error
}

type ArchiveImpl struct {
	repos    store.RepoStore
	logs     store.LogStore
	settings store.SettingsStore
	client   github.Client
}

var _ ArchiveWorkflow = (*ArchiveImpl)(nil)

func NewArchive(repos store.RepoStore, logs store.LogStore, settings store.SettingsStore, client github.Client) *ArchiveImpl {
	return &ArchiveImpl{
		repos:    repos,
		logs:     logs,
		settings: settings,
		client:   client,
	}
}

func (a *ArchiveImpl) RunEligible(ctx context.Context) error {
	repos, err := a.repos.List()
	if err != nil {
		return fmt.Errorf("workflow: list repos: %w", err)
	}

	settings, err := a.settings.GetAll()
	if err != nil {
		return fmt.Errorf("workflow: get settings: %w", err)
	}

	if settings.AutoArchiveDays <= 0 {
		slog.Info("workflow: auto archive disabled (auto_archive_days is non-positive), skipping archive run")
		return nil
	}

	threshold := time.Duration(settings.AutoArchiveDays) * 24 * time.Hour

	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("workflow: archive run cancelled: %w", err)
		}

		logger := slog.With("repo_id", repo.ID, "owner", repo.Owner, "name", repo.Name)

		if repo.GitHubArchived || repo.GitHubDeleted {
			continue
		}
		if !repo.AutoArchive {
			continue
		}
		if repo.LastPush == nil {
			continue
		}
		if repo.VerifiedAt == nil {
			continue
		}
		if repo.BackupPath == nil {
			continue
		}

		if time.Since(*repo.LastPush) <= threshold {
			continue
		}

		if settings.DryRun {
			logger.Info("workflow: would archive (dry-run)")
			if logErr := a.logs.Create(model.LogEntry{
				RepoID:  repo.ID,
				Action:  "archive",
				Status:  "dry-run",
				Message: fmt.Sprintf("would archive %s/%s (dry-run)", repo.Owner, repo.Name),
			}); logErr != nil {
				logger.Error("workflow: create log entry", "error", logErr)
			}
			continue
		}

		if err := a.client.ArchiveRepo(ctx, repo.Owner, repo.Name); err != nil {
			logger.Error("workflow: archive repo", "error", err)
			if logErr := a.logs.Create(model.LogEntry{
				RepoID:  repo.ID,
				Action:  "archive",
				Status:  "error",
				Message: err.Error(),
			}); logErr != nil {
				logger.Error("workflow: create log entry", "error", logErr)
			}
			continue
		}

		logger.Info("workflow: repo archived")
		if err := a.logs.Create(model.LogEntry{
			RepoID:  repo.ID,
			Action:  "archive",
			Status:  "success",
			Message: fmt.Sprintf("archived %s/%s", repo.Owner, repo.Name),
		}); err != nil {
			logger.Error("workflow: create log entry", "error", err)
		}

		var lang, url *string
		if repo.Language != nil {
			v := *repo.Language
			lang = &v
		}
		if repo.GitHubURL != nil {
			v := *repo.GitHubURL
			url = &v
		}
		if err := a.repos.SetGitHubMetadata(repo.ID, repo.SizeKB, lang, url, true, repo.LastPush); err != nil {
			logger.Error("workflow: set github metadata", "error", err)
		}
	}

	return nil
}
