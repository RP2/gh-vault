package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-chi/chi/v5"
	gh "github.com/google/go-github/v69/github"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"

	"github.com/RP2/gh-vault/internal/model"
	"github.com/RP2/gh-vault/internal/store"
)

// rate limit cache for token status endpoint
var (
	cachedRateLimit   *gh.Rate
	cachedRateLimitAt time.Time
	rateLimitMu       sync.Mutex
	rateLimitTTL      = 60 * time.Second

	syncRunning    atomic.Bool
	lastSyncResult syncResult
	syncResultMu   sync.Mutex
)

type syncResult struct {
	Done    bool
	Added   int
	Updated int
	Deleted int
	Error   string
}

// parseRepoID parses the {id} URL parameter as an int64.
func parseRepoID(r *http.Request) (int64, error) {
	idStr := chi.URLParam(r, "id")
	id, err := strconv.ParseInt(idStr, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid repo id %q: %w", idStr, err)
	}
	return id, nil
}

// csrfToken returns the CSRF token for the current session, if any.
func (s *Server) csrfToken(r *http.Request) string {
	if session := s.getSession(r); session != nil {
		return session.CSRFToken
	}
	return ""
}

// renderTemplate renders a pre-parsed page template wrapped in the layout.
func (s *Server) renderTemplate(w http.ResponseWriter, name string, data any) {
	t, ok := s.templates[name]
	if !ok {
		slog.Error("template not found", "name", name)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if err := t.ExecuteTemplate(w, "layout", data); err != nil {
		slog.Error("render template", "template", name, "error", err)
	}
}

// createLogEntry creates a log entry and logs any failure.
func (s *Server) createLogEntry(repoID int64, action, status, message string) {
	if err := s.logs.Create(model.LogEntry{
		RepoID:  repoID,
		Action:  action,
		Status:  status,
		Message: message,
	}); err != nil {
		slog.Error("create log entry", "error", err)
	}
}

// getRateLimit returns a cached rate limit or fetches a fresh one.
func (s *Server) getRateLimit(ctx context.Context) (*gh.Rate, error) {
	rateLimitMu.Lock()
	defer rateLimitMu.Unlock()

	if cachedRateLimit != nil && time.Since(cachedRateLimitAt) < rateLimitTTL {
		return cachedRateLimit, nil
	}

	rate, err := s.ghClient.RateLimitStatus(ctx)
	if err != nil {
		return nil, err
	}
	cachedRateLimit = rate
	cachedRateLimitAt = time.Now()
	return rate, nil
}

// clearRateLimitCache resets the rate limit cache. Call after token rotation so
// the next status request reflects the new token's quota instead of a stale
// entry from the previous identity.
func clearRateLimitCache() {
	rateLimitMu.Lock()
	defer rateLimitMu.Unlock()
	cachedRateLimit = nil
	cachedRateLimitAt = time.Time{}
}

// background returns a detached context with a timeout suitable for async work.
func background() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 30*time.Minute)
}

// Setup Wizard

func (s *Server) handleSetupGet(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "setup", map[string]string{
		"CSRFToken": s.csrfToken(r),
	})
}

func (s *Server) handleSetupPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
		if username == "" || len(password) < 8 {
			s.renderTemplate(w, "setup", map[string]string{
				"Error":     "username is required and password must be at least 8 characters",
				"CSRFToken": s.csrfToken(r),
			})
			return
		}

	hash, err := bcrypt.GenerateFromPassword([]byte(password), 12)
	if err != nil {
		slog.Error("hash password", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	userID, err := s.users.Create(username, string(hash))
	if err != nil {
		slog.Error("create user", "username", username, "error", err)
		if errors.Is(err, store.ErrUsernameExists) {
			s.renderTemplate(w, "setup", map[string]string{
				"Error":     "username already exists",
				"CSRFToken": s.csrfToken(r),
			})
			return
		}
		if errors.Is(err, store.ErrSingleUserOnly) {
			s.renderTemplate(w, "setup", map[string]string{
				"Error":     "an account already exists for this deployment",
				"CSRFToken": s.csrfToken(r),
			})
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	s.setupDone = true

	if err := s.createSession(w, userID); err != nil {
		slog.Error("create session", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/settings?reason=token_missing", http.StatusFound)
}

// Auth

func (s *Server) handleLoginGet(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "login", map[string]string{
		"Error":     r.URL.Query().Get("error"),
		"CSRFToken": s.csrfToken(r),
	})
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	username := r.FormValue("username")
	password := r.FormValue("password")

	user, err := s.users.GetByUsername(username)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// Dummy bcrypt work to prevent username enumeration via timing.
			dummyHash, _ := bcrypt.GenerateFromPassword([]byte("dummy"), 12)
			bcrypt.CompareHashAndPassword(dummyHash, []byte(password))
			http.Redirect(w, r, "/login?error=invalid", http.StatusFound)
			return
		}
		slog.Error("get user", "username", username, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if err := bcrypt.CompareHashAndPassword([]byte(user.PasswordHash), []byte(password)); err != nil {
		http.Redirect(w, r, "/login?error=invalid", http.StatusFound)
		return
	}

	if err := s.createSession(w, user.ID); err != nil {
		slog.Error("create session", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if err := s.destroySession(w, r); err != nil {
		slog.Error("destroy session", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/login", http.StatusFound)
}

// Dashboard

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	repos, err := s.repos.List()
	if err != nil {
		slog.Error("list repos for dashboard", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	// Use a bounded recent activity feed instead of loading the entire log table.
	logs, err := s.logs.Recent(100)
	if err != nil {
		slog.Error("list logs for dashboard", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	data := struct {
		RepoCount   int
		LogCount    int
		SyncRunning bool
		NextSync    time.Time
		CSRFToken   string
	}{
		RepoCount:   len(repos),
		LogCount:    len(logs),
		SyncRunning: s.sched.IsRunning("sync"),
		NextSync:    s.sched.NextRun("sync"),
		CSRFToken:   s.csrfToken(r),
	}

	s.renderTemplate(w, "dashboard", data)
}

// Repos

func (s *Server) handleReposList(w http.ResponseWriter, r *http.Request) {
	repos, err := s.repos.List()
	if err != nil {
		slog.Error("list repos", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	var active, deleted []model.Repo
	for _, repo := range repos {
		if repo.GitHubDeleted {
			deleted = append(deleted, repo)
		} else {
			active = append(active, repo)
		}
	}

	data := struct {
		CSRFToken    string
		Repos        []model.Repo
		DeletedRepos []model.Repo
		DeletedCount int
	}{
		CSRFToken:    s.csrfToken(r),
		Repos:        active,
		DeletedRepos: deleted,
		DeletedCount: len(deleted),
	}
	s.renderTemplate(w, "repos", data)
}

func (s *Server) handleRepoSwitch(w http.ResponseWriter, r *http.Request) {
	id, err := parseRepoID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	repo, err := s.repos.Get(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.Error("get repo", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	ctx := r.Context()
	switch repo.Format {
	case model.FormatClone:
		err = s.engine.SwitchToBundle(ctx, repo)
	default:
		err = s.engine.SwitchToClone(ctx, repo)
	}
	if err != nil {
		slog.Error("switch repo format", "owner", repo.Owner, "name", repo.Name, "format", repo.Format, "error", err)
		http.Error(w, "Failed to switch format", http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func (s *Server) handleRepoArchive(w http.ResponseWriter, r *http.Request) {
	id, err := parseRepoID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	repo, err := s.repos.Get(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.Error("get repo", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if repo.BackupPath == nil || repo.VerifiedAt == nil {
		http.Error(w, "backup must exist and be verified before archiving", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if err := s.ghClient.ArchiveRepo(ctx, repo.Owner, repo.Name); err != nil {
		slog.Error("archive repo", "owner", repo.Owner, "name", repo.Name, "error", err)
		s.createLogEntry(repo.ID, "archive", "error", err.Error())
		http.Error(w, "Failed to archive repository", http.StatusInternalServerError)
		return
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
	if err := s.repos.SetGitHubMetadata(repo.ID, repo.SizeKB, lang, url, true, repo.LastPush); err != nil {
		slog.Error("set github metadata after archive", "id", repo.ID, "error", err)
	}

	s.createLogEntry(repo.ID, "archive", "success", "repository archived on GitHub")
	http.Redirect(w, r, "/repos", http.StatusFound)
}

func (s *Server) handleRepoDelete(w http.ResponseWriter, r *http.Request) {
	id, err := parseRepoID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	repo, err := s.repos.Get(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.Error("get repo", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if r.PostFormValue("name") != repo.Name {
		http.Error(w, "repository name mismatch", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	if err := s.ghClient.DeleteRepo(ctx, repo.Owner, repo.Name); err != nil {
		slog.Error("delete repo", "owner", repo.Owner, "name", repo.Name, "error", err)
		s.createLogEntry(repo.ID, "delete", "error", err.Error())
		http.Error(w, "Failed to delete repository", http.StatusInternalServerError)
		return
	}

	if err := s.repos.MarkDeleted(repo.ID); err != nil {
		slog.Error("mark repo deleted", "id", repo.ID, "error", err)
		// Retry once: the GitHub deletion already succeeded, so we must not
		// return a 500 and leave the local state orphaned.
		if err2 := s.repos.MarkDeleted(repo.ID); err2 != nil {
			slog.Error("mark repo deleted retry failed", "id", repo.ID, "error", err2)
			s.createLogEntry(repo.ID, "delete", "error", "GitHub delete succeeded but local mark failed: "+err2.Error())
			http.Redirect(w, r, "/repos", http.StatusFound)
			return
		}
	}

	s.createLogEntry(repo.ID, "delete", "success", "repository deleted on GitHub")
	http.Redirect(w, r, "/repos", http.StatusFound)
}

func (s *Server) handleRepoDeletePermanent(w http.ResponseWriter, r *http.Request) {
	id, err := parseRepoID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	repo, err := s.repos.Get(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.Error("get repo", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if r.PostFormValue("name") != repo.Name {
		http.Error(w, "repository name mismatch", http.StatusBadRequest)
		return
	}

	if err := s.repos.Delete(repo.ID); err != nil {
		slog.Error("permanently delete repo", "id", repo.ID, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/repos", http.StatusFound)
}

func (s *Server) handleRepoBackup(w http.ResponseWriter, r *http.Request) {
	id, err := parseRepoID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	repo, err := s.repos.Get(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.Error("get repo", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if repo.GitHubDeleted {
		http.Error(w, "repository already deleted on GitHub", http.StatusConflict)
		return
	}

	w.WriteHeader(http.StatusAccepted)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in background job", "panic", r)
			}
		}()
		ctx, cancel := background()
		defer cancel()

		// Re-read the repo from the DB so we act on fresh state — a concurrent
		// sync or archive may have flipped GitHubDeleted between request
		// acceptance and the goroutine starting.
		repo2, err := s.repos.Get(repo.ID)
		if err != nil || repo2.GitHubDeleted {
			return
		}
		repo = repo2

		if err := s.engine.CloneRepo(ctx, repo); err != nil {
			slog.Error("backup repo", "owner", repo.Owner, "name", repo.Name, "error", err)
			s.createLogEntry(repo.ID, "backup", "error", err.Error())
			return
		}

		now := time.Now()
		if err := s.repos.SetLastBackup(repo.ID, &now); err != nil {
			slog.Error("set last backup", "id", repo.ID, "error", err)
		}
		s.createLogEntry(repo.ID, "backup", "success", "manual backup completed")
	}()
}

func (s *Server) handleRepoAutoArchive(w http.ResponseWriter, r *http.Request) {
	id, err := parseRepoID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	repo, err := s.repos.Get(id)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			http.NotFound(w, r)
			return
		}
		slog.Error("get repo", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	desired := r.FormValue("auto_archive") == "on"
	if err := s.repos.SetAutoArchive(repo.ID, desired); err != nil {
		slog.Error("set auto archive", "id", repo.ID, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/repos", http.StatusFound)
}

func (s *Server) handleRepoBackupToggle(w http.ResponseWriter, r *http.Request) {
	id, err := parseRepoID(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	desired := r.FormValue("backup_enabled") == "on"
	if err := s.repos.SetBackupEnabled(id, desired); err != nil {
		slog.Error("set backup enabled", "id", id, "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
}

// Triggers

func (s *Server) handleTriggerSync(w http.ResponseWriter, r *http.Request) {
	if syncRunning.Load() {
		w.Header().Set("Content-Type", "text/html")
		w.Write([]byte(`<span class="badge">Sync already running</span>`))
		return
	}
	syncRunning.Store(true)
	syncResultMu.Lock()
	lastSyncResult = syncResult{}
	syncResultMu.Unlock()

	go func() {
		defer syncRunning.Store(false)
		ctx, cancel := background()
		defer cancel()
		result, err := s.syncer.SyncRepos(ctx)
		syncResultMu.Lock()
		if err != nil {
			lastSyncResult = syncResult{Done: true, Error: err.Error()}
		} else {
			lastSyncResult = syncResult{Done: true, Added: result.Added, Updated: result.Updated, Deleted: result.Deleted}
		}
		syncResultMu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/html")
	w.WriteHeader(http.StatusAccepted)
	fmt.Fprint(w, `<div id="sync-status" hx-get="/sync/status" hx-trigger="every 2s" hx-swap="outerHTML"><span class="spinner">Syncing from GitHub...</span></div>`)
}

func (s *Server) handleSyncStatus(w http.ResponseWriter, r *http.Request) {
	syncResultMu.Lock()
	result := lastSyncResult
	syncResultMu.Unlock()

	w.Header().Set("Content-Type", "text/html")

	if syncRunning.Load() {
		fmt.Fprint(w, `<div id="sync-status" hx-get="/sync/status" hx-trigger="every 2s" hx-swap="outerHTML"><span class="spinner">Syncing from GitHub...</span></div>`)
		return
	}

	if !result.Done {
		fmt.Fprint(w, `<div id="sync-status"></div>`)
		return
	}

	if result.Error != "" {
		fmt.Fprintf(w, `<div id="sync-status" class="alert error"><span>Sync failed: %s</span></div>`, template.HTMLEscapeString(result.Error))
		return
	}

	fmt.Fprintf(w, `<div id="sync-status"><span>Sync complete: %d added, %d updated, %d deleted</span> <button onclick="location.reload()">Refresh</button></div>`, result.Added, result.Updated, result.Deleted)
}

func (s *Server) handleTriggerBackup(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusAccepted)

	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				slog.Error("panic in background job", "panic", rec)
			}
		}()
		ctx, cancel := background()
		defer cancel()

		repos, err := s.repos.List()
		if err != nil {
			slog.Error("list repos for trigger backup", "error", err)
			return
		}

		for _, repo := range repos {
			if repo.GitHubDeleted || !repo.BackupEnabled {
				continue
			}
			if err := s.engine.CloneRepo(ctx, repo); err != nil {
				slog.Error("backup repo", "owner", repo.Owner, "name", repo.Name, "error", err)
				s.createLogEntry(repo.ID, "backup", "error", err.Error())
				continue
			}
			now := time.Now()
			if err := s.repos.SetLastBackup(repo.ID, &now); err != nil {
				slog.Error("set last backup", "id", repo.ID, "error", err)
			}
			s.createLogEntry(repo.ID, "backup", "success", "triggered backup completed")
		}
	}()
}

// Logs

func (s *Server) handleLogs(w http.ResponseWriter, r *http.Request) {
	page := 1
	size := 50

	if p := r.URL.Query().Get("page"); p != "" {
		if n, err := strconv.Atoi(p); err == nil && n > 0 {
			page = n
		}
	}
	if sz := r.URL.Query().Get("size"); sz != "" {
		if n, err := strconv.Atoi(sz); err == nil && n > 0 {
			size = n
		}
	}
	if size > 500 {
		size = 500
	}
	if page > 1000 {
		page = 1000
	}

	// Implement pagination without changing the LogStore interface: fetch
	// enough entries to cover the requested page, then slice.
	total := page * size
	if total > 5000 {
		total = 5000
	}
	entries, err := s.logs.Recent(total)
	if err != nil {
		slog.Error("list logs", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	start := (page - 1) * size
	if start > len(entries) {
		start = len(entries)
	}
	pageEntries := entries[start:]
	if len(pageEntries) > size {
		pageEntries = pageEntries[:size]
	}

	data := struct {
		Logs      []model.LogEntry
		Page      int
		Size      int
		PrevPage  int
		NextPage  int
		HasNext   bool
		CSRFToken string
	}{
		Logs:      pageEntries,
		Page:      page,
		Size:      size,
		PrevPage:  page - 1,
		NextPage:  page + 1,
		HasNext:   len(entries) == total,
		CSRFToken: s.csrfToken(r),
	}

	s.renderTemplate(w, "logs", data)
}

// Settings

func (s *Server) handleSettingsGet(w http.ResponseWriter, r *http.Request) {
	settings, err := s.settings.GetAll()
	if err != nil {
		slog.Error("get settings", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	data := struct {
		Settings  model.Settings
		Reason    string
		CSRFToken string
	}{
		Settings:  settings,
		Reason:    r.URL.Query().Get("reason"),
		CSRFToken: s.csrfToken(r),
	}

	s.renderTemplate(w, "settings", data)
}

func (s *Server) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	cron := r.FormValue("cron_schedule")
	dryRun := r.FormValue("dry_run")
	autoArchiveDays := r.FormValue("auto_archive_days")
	logRetentionDays := r.FormValue("log_retention_days")

	dryRunStr := "false"
	if dryRun == "on" || dryRun == "true" {
		dryRunStr = "true"
	}

	currentSettings, err := s.settings.GetAll()
	if err != nil {
		slog.Error("get settings", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if err := s.settings.Set("cron_schedule", cron); err != nil {
		slog.Error("set cron_schedule", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if err := s.settings.Set("dry_run", dryRunStr); err != nil {
		slog.Error("set dry_run", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if err := s.settings.Set("auto_archive_days", autoArchiveDays); err != nil {
		slog.Error("set auto_archive_days", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	if err := s.settings.Set("log_retention_days", logRetentionDays); err != nil {
		slog.Error("set log_retention_days", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	if cron != currentSettings.CronSchedule {
		if err := s.sched.ReloadCron(cron); err != nil {
			slog.Error("reload cron", "expr", cron, "error", err)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
	}

	http.Redirect(w, r, "/settings", http.StatusFound)
}

func (s *Server) handleSettingsTokenPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	token := r.FormValue("token")
	if token == "" {
		http.Error(w, "token is required", http.StatusBadRequest)
		return
	}

	ctx := r.Context()
	// Validate the NEW token with a one-shot client. Using s.ghClient here
	// would reuse the previously-cached token and fail on first-time setup.
	src := oauth2.StaticTokenSource(&oauth2.Token{AccessToken: token})
	httpClient := oauth2.NewClient(ctx, src)
	ghClient := gh.NewClient(httpClient)
	if _, _, err := ghClient.Repositories.ListByAuthenticatedUser(ctx, &gh.RepositoryListByAuthenticatedUserOptions{
		ListOptions: gh.ListOptions{PerPage: 1},
	}); err != nil {
		slog.Error("validate token", "error", err)
		http.Error(w, "Invalid token", http.StatusBadRequest)
		return
	}

	if err := s.tokenProvider.SetToken(ctx, token); err != nil {
		slog.Error("set token", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	clearRateLimitCache()

	if err := s.settings.Set("token_last_validated", time.Now().UTC().Format(time.RFC3339)); err != nil {
		slog.Error("set token_last_validated", "error", err)
	}

	http.Redirect(w, r, "/", http.StatusFound)
}

func (s *Server) handleSettingsTokenStatus(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	token, err := s.tokenProvider.GetToken(ctx)
	set := err == nil && token != ""
	if err != nil {
		slog.Error("get token status", "error", err)
	}

	lastValidated, _ := s.settings.Get("token_last_validated")

	rate, err := s.getRateLimit(ctx)
	if err != nil {
		slog.Error("get rate limit", "error", err)
	}

	resp := struct {
		Set           bool     `json:"set"`
		LastValidated string   `json:"last_validated"`
		RateLimit     *gh.Rate `json:"rate_limit"`
	}{
		Set:           set,
		LastValidated: lastValidated,
		RateLimit:     rate,
	}

	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		slog.Error("encode token status", "error", err)
	}
}
