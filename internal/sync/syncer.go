package sync

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	gh "github.com/google/go-github/v69/github"

	"github.com/RP2/gh-vault/internal/github"
	"github.com/RP2/gh-vault/internal/model"
	"github.com/RP2/gh-vault/internal/store"
)

// Syncer coordinates repository metadata between GitHub and the local store.
type Syncer interface {
	SyncRepos(ctx context.Context) (SyncResult, error)
}

// SyncResult reports the outcome of a repository sync.
type SyncResult struct {
	Added       int
	Updated     int
	Renamed     int
	Transferred int
	Deleted     int
	Unchanged   int
	ErrorCount  int
}

// SyncerImpl implements Syncer.
type SyncerImpl struct {
	client github.Client
	repos  store.RepoStore
	logs   store.LogStore
}

var _ Syncer = (*SyncerImpl)(nil)

// NewSyncer creates a new SyncerImpl.
func NewSyncer(client github.Client, repos store.RepoStore, logs store.LogStore) *SyncerImpl {
	return &SyncerImpl{
		client: client,
		repos:  repos,
		logs:   logs,
	}
}

// SyncRepos reconciles the local repository store with the repositories returned
// by the GitHub API.
func (s *SyncerImpl) SyncRepos(ctx context.Context) (SyncResult, error) {
	var result SyncResult

	ghRepos, err := s.client.ListAccessibleRepos(ctx)
	if err != nil {
		return result, fmt.Errorf("sync: list accessible repos: %w", err)
	}

	storedRepos, err := s.repos.List()
	if err != nil {
		return result, fmt.Errorf("sync: list stored repos: %w", err)
	}

	if len(ghRepos) == 0 && len(storedRepos) > 0 {
		slog.Warn("sync: API returned 0 repos but store has repos — token may lack permissions",
			"store_repos", len(storedRepos))
	}

	slog.Info("sync: starting", "api_repos", len(ghRepos), "store_repos", len(storedRepos))

	ghMap := make(map[int64]*gh.Repository, len(ghRepos))
	for _, r := range ghRepos {
		ghMap[r.GetID()] = r
	}

	storeMap := make(map[int64]model.Repo, len(storedRepos))
	for _, r := range storedRepos {
		storeMap[r.GitHubID] = r
	}

	for _, ghRepo := range ghRepos {
		owner := ghRepo.GetOwner().GetLogin()
		name := ghRepo.GetName()
		ghID := ghRepo.GetID()

		logger := slog.With("github_id", ghID, "owner", owner, "name", name)

		stored, ok := storeMap[ghID]
		if !ok {
			if err := s.addNewRepo(ghRepo, &result); err != nil {
				logger.Error("sync: add new repo", "error", err)
				result.ErrorCount++
				continue
			}
			logger.Info("sync: added new repo", "repo", owner+"/"+name)
			result.Added++
			continue
		}

		newURL := fmt.Sprintf("https://github.com/%s/%s", owner, name)
		newBackupPath := fmt.Sprintf("active/%s/%s.git", owner, name)
		metadata := repoMetadataFromGitHub(ghRepo, newURL)

		ownerChanged := stored.Owner != owner
		nameChanged := stored.Name != name

		if ownerChanged || nameChanged {
			if err := s.repos.SetOwnerName(stored.ID, owner, name, newURL); err != nil {
				logger.Error("sync: set owner/name", "error", err)
				result.ErrorCount++
				continue
			}
			if err := s.repos.SetBackupPath(stored.ID, newBackupPath); err != nil {
				logger.Error("sync: set backup path", "error", err)
				result.ErrorCount++
				continue
			}

			action := "sync.renamed"
			if ownerChanged {
				action = "sync.transferred"
				result.Transferred++
			} else if nameChanged {
				result.Renamed++
			}

			if err := s.logs.Create(model.LogEntry{
				RepoID:  stored.ID,
				Action:  action,
				Status:  "success",
				Message: fmt.Sprintf("repo changed to %s/%s", owner, name),
			}); err != nil {
				logger.Error("sync: create log entry", "error", err)
				result.ErrorCount++
			}
			continue
		}

		if metadataChanged(stored, metadata) {
			if stored.BackupPath == nil || *stored.BackupPath != newBackupPath {
				if err := s.repos.SetBackupPath(stored.ID, newBackupPath); err != nil {
					logger.Error("sync: set backup path", "error", err)
					result.ErrorCount++
					continue
				}
			}
			if err := s.repos.SetGitHubMetadata(
				stored.ID,
				metadata.sizeKB,
				metadata.language,
				metadata.url,
				metadata.archived,
				metadata.lastPush,
			); err != nil {
				logger.Error("sync: set github metadata", "error", err)
				result.ErrorCount++
				continue
			}
			result.Updated++
			if err := s.logs.Create(model.LogEntry{
				RepoID:  stored.ID,
				Action:  "sync.updated",
				Status:  "success",
				Message: "metadata updated",
			}); err != nil {
				logger.Error("sync: create log entry", "error", err)
				result.ErrorCount++
			}
			continue
		}

		result.Unchanged++
	}

	for _, stored := range storedRepos {
		if _, ok := ghMap[stored.GitHubID]; ok {
			continue
		}

		logger := slog.With("repo_id", stored.ID, "owner", stored.Owner, "name", stored.Name)

		slog.Warn("sync: repo not in GitHub API, marking removed", "repo", stored.Owner+"/"+stored.Name, "github_id", stored.GitHubID)

		if err := s.repos.MarkDeleted(stored.ID); err != nil {
			logger.Error("sync: mark removed", "error", err)
			result.ErrorCount++
			continue
		}
		if err := s.logs.Create(model.LogEntry{
			RepoID:  stored.ID,
			Action:  "sync.removed",
			Status:  "success",
			Message: fmt.Sprintf("removed %s/%s from GitHub", stored.Owner, stored.Name),
		}); err != nil {
			logger.Error("sync: create log entry", "error", err)
			result.ErrorCount++
		}
		result.Deleted++
	}

	slog.Info("sync: complete", "added", result.Added, "updated", result.Updated, "removed", result.Deleted, "unchanged", result.Unchanged)

	return result, nil
}

type repoMetadata struct {
	sizeKB   int64
	language *string
	url      *string
	archived bool
	lastPush *time.Time
}

func repoMetadataFromGitHub(r *gh.Repository, url string) repoMetadata {
	var language *string
	if r.GetLanguage() != "" {
		l := r.GetLanguage()
		language = &l
	}

	var lastPush *time.Time
	if r.PushedAt != nil {
		lastPush = r.PushedAt.GetTime()
	}

	return repoMetadata{
		sizeKB:   int64(r.GetSize()),
		language: language,
		url:      &url,
		archived: r.GetArchived(),
		lastPush: lastPush,
	}
}

func metadataChanged(stored model.Repo, metadata repoMetadata) bool {
	if stored.SizeKB != metadata.sizeKB {
		return true
	}
	if stored.GitHubArchived != metadata.archived {
		return true
	}
	if !ptrStringEqual(stored.Language, metadata.language) {
		return true
	}
	if !ptrStringEqual(stored.GitHubURL, metadata.url) {
		return true
	}
	if !ptrTimeEqual(stored.LastPush, metadata.lastPush) {
		return true
	}
	expectedBackupPath := fmt.Sprintf("active/%s/%s.git", stored.Owner, stored.Name)
	if stored.BackupPath == nil || *stored.BackupPath != expectedBackupPath {
		return true
	}
	return false
}

func ptrStringEqual(a, b *string) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

func ptrTimeEqual(a, b *time.Time) bool {
	if a == nil || b == nil {
		return a == b
	}
	return a.Equal(*b)
}

func (s *SyncerImpl) addNewRepo(ghRepo *gh.Repository, result *SyncResult) error {
	owner := ghRepo.GetOwner().GetLogin()
	name := ghRepo.GetName()
	url := fmt.Sprintf("https://github.com/%s/%s", owner, name)
	backupPath := fmt.Sprintf("active/%s/%s.git", owner, name)
	metadata := repoMetadataFromGitHub(ghRepo, url)

	newRepo := model.Repo{
		GitHubID:       ghRepo.GetID(),
		Owner:          owner,
		Name:           name,
		Format:         model.FormatClone,
		BackupPath:     &backupPath,
		GitHubURL:      metadata.url,
		Language:       metadata.language,
		SizeKB:         metadata.sizeKB,
		LastPush:       metadata.lastPush,
		GitHubArchived: metadata.archived,
		AutoArchive:    false,
	}

	id, err := s.repos.Upsert(newRepo)
	if err != nil {
		return fmt.Errorf("upsert repo: %w", err)
	}

	if err := s.logs.Create(model.LogEntry{
		RepoID:  id,
		Action:  "sync.added",
		Status:  "success",
		Message: fmt.Sprintf("added %s/%s", owner, name),
	}); err != nil {
		slog.Error("sync: create log entry", "github_id", ghRepo.GetID(), "owner", owner, "name", name, "error", err)
		result.ErrorCount++
	}

	return nil
}
