package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"
	gh "github.com/google/go-github/v69/github"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/oauth2"

	"github.com/RP2/gh-vault/internal/model"
	"github.com/RP2/gh-vault/internal/store"
	syncpkg "github.com/RP2/gh-vault/internal/sync"
)

// rate limit cache for token status endpoint
var (
	cachedRateLimit   *gh.Rate
	cachedRateLimitAt time.Time
	rateLimitMu       sync.Mutex
	rateLimitTTL      = 60 * time.Second
)

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

// isHtmx reports whether the request was issued by htmx.
func isHtmx(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}

// Setup Wizard

func (s *Server) handleSetupGet(w http.ResponseWriter, r *http.Request) {
	s.renderTemplate(w, "setup", map[string]string{
		"CSRFToken":   s.csrfToken(r),
		"CurrentPath": r.URL.Path,
	})
}

func (s *Server) handleSetupPost(w http.ResponseWriter, r *http.Request) {
	if !s.rateLimiter.allow(clientIP(r.RemoteAddr)) {
		http.Error(w, "Too many attempts", http.StatusTooManyRequests)
		return
	}

	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	username := strings.TrimSpace(r.FormValue("username"))
	password := r.FormValue("password")
	if username == "" || len(password) < 8 {
		s.renderTemplate(w, "setup", map[string]string{
			"Error":       "username is required and password must be at least 8 characters",
			"CSRFToken":   s.csrfToken(r),
			"CurrentPath": r.URL.Path,
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
				"Error":       "username already exists",
				"CSRFToken":   s.csrfToken(r),
				"CurrentPath": r.URL.Path,
			})
			return
		}
		if errors.Is(err, store.ErrSingleUserOnly) {
			s.renderTemplate(w, "setup", map[string]string{
				"Error":       "an account already exists for this deployment",
				"CSRFToken":   s.csrfToken(r),
				"CurrentPath": r.URL.Path,
			})
			return
		}
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	s.setupDone = true
	s.rateLimiter.reset(clientIP(r.RemoteAddr))

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
		"Error":       r.URL.Query().Get("error"),
		"CSRFToken":   s.csrfToken(r),
		"CurrentPath": r.URL.Path,
	})
}

func (s *Server) handleLoginPost(w http.ResponseWriter, r *http.Request) {
	ip := clientIP(r.RemoteAddr)
	if !s.rateLimiter.allow(ip) {
		http.Redirect(w, r, "/login?error=locked", http.StatusFound)
		return
	}

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

	s.rateLimiter.reset(ip)

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

	logs, err := s.logs.Recent(5)
	if err != nil {
		slog.Error("list logs for dashboard", "error", err)
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}

	var total, backedUp, notCopied int
	for _, repo := range repos {
		if repo.GitHubDeleted {
			continue
		}
		total++
		if repo.LastBackup != nil {
			backedUp++
		} else {
			notCopied++
		}
	}

	data := map[string]any{
		"RepoCount":   total,
		"BackedUp":    backedUp,
		"NotBackedUp": notCopied,
		"RecentLogs":  logs,
		"CSRFToken":   s.csrfToken(r),
		"CurrentPath": r.URL.Path,
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
		CurrentPath  string
	}{
		CSRFToken:    s.csrfToken(r),
		Repos:        active,
		DeletedRepos: deleted,
		DeletedCount: len(deleted),
		CurrentPath:  r.URL.Path,
	}
	s.renderTemplate(w, "repos", data)
}

func (s *Server) renderReposTbody(w io.Writer, r *http.Request) error {
	repos, err := s.repos.List()
	if err != nil {
		return err
	}
	var active []model.Repo
	for _, repo := range repos {
		if !repo.GitHubDeleted {
			active = append(active, repo)
		}
	}
	t, ok := s.templates["repos"]
	if !ok {
		return errors.New("repos template not found")
	}
	return t.ExecuteTemplate(w, "repos-tbody", map[string]any{"Repos": active})
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

func (s *Server) handleBulkSetFormat(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	idsStr := strings.Trim(r.FormValue("ids"), ",")
	format := r.FormValue("format") // "clone" or "bundle"
	if format != "clone" && format != "bundle" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	ids := strings.Split(idsStr, ",")
	if len(ids) > 200 {
		http.Error(w, "too many selections (max 200)", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(ids[0]) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, idStr := range ids {
		id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
		if err != nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		repo, err := s.repos.Get(id)
		if err != nil {
			slog.Error("bulk set format: lookup", "id", id, "error", err)
			continue
		}
		if repo.GitHubDeleted {
			continue
		}
		seen[id] = struct{}{}
		backupPath := syncpkg.PathForFormat(model.RepoFormat(format), repo.Owner, repo.Name)
		if err := s.repos.SetFormat(id, model.RepoFormat(format), backupPath); err != nil {
			slog.Error("bulk set format", "id", id, "error", err)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleBulkSetBackup(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	idsStr := strings.Trim(r.FormValue("ids"), ",")
	ids := strings.Split(idsStr, ",")
	enabled := r.FormValue("backup_enabled") == "on"
	if len(ids) > 200 {
		http.Error(w, "too many selections (max 200)", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(ids[0]) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, idStr := range ids {
		id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
		if err != nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		repo, err := s.repos.Get(id)
		if err != nil {
			slog.Error("lookup repo", "id", id, "error", err)
			continue
		}
		if repo.GitHubDeleted {
			continue
		}
		seen[id] = struct{}{}
		if err := s.repos.SetBackupEnabled(id, enabled); err != nil {
			slog.Error("bulk set backup", "id", id, "error", err)
		}
	}
	w.WriteHeader(http.StatusAccepted)
}

func (s *Server) handleBackupChecked(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	idsStr := strings.Trim(r.FormValue("ids"), ",")
	ids := strings.Split(idsStr, ",")
	if len(ids) > 200 {
		http.Error(w, "too many selections (max 200)", http.StatusBadRequest)
		return
	}
	if strings.TrimSpace(ids[0]) == "" {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	seen := make(map[int64]struct{}, len(ids))
	for _, idStr := range ids {
		id, err := strconv.ParseInt(strings.TrimSpace(idStr), 10, 64)
		if err != nil {
			continue
		}
		if _, dup := seen[id]; dup {
			continue
		}
		repo, err := s.repos.Get(id)
		if err != nil {
			slog.Error("lookup repo", "id", id, "error", err)
			continue
		}
		if repo.GitHubDeleted {
			continue
		}
		if !repo.BackupEnabled {
			continue
		}
		seen[id] = struct{}{}
		go func(repoID int64) {
			ctx, cancel := background()
			defer cancel()

			// Re-read the repo from the DB so we act on fresh state.
			r, err := s.repos.Get(repoID)
			if err != nil || r.GitHubDeleted || !r.BackupEnabled {
				return
			}

			if r.Format == model.FormatBundle {
				if err := s.engine.CloneRepo(ctx, r); err != nil {
					slog.Error("backup checked", "repo", r.Name, "error", err)
					s.createLogEntry(r.ID, "backup", "error", err.Error())
					return
				}
				if err := s.engine.CreateBundle(ctx, r); err != nil {
					slog.Error("create bundle checked", "repo", r.Name, "error", err)
					s.createLogEntry(r.ID, "backup", "error", err.Error())
					return
				}
			} else {
				if err := s.engine.CloneRepo(ctx, r); err != nil {
					slog.Error("backup checked", "repo", r.Name, "error", err)
					s.createLogEntry(r.ID, "backup", "error", err.Error())
					return
				}
			}
			now := time.Now()
			if err := s.repos.SetLastBackup(r.ID, &now); err != nil {
				slog.Error("set last backup", "id", r.ID, "error", err)
			}
			s.createLogEntry(r.ID, "backup", "success", "checked backup completed")
		}(repo.ID)
	}
	w.WriteHeader(http.StatusAccepted)
}

// Triggers

func (s *Server) handleTriggerSync(w http.ResponseWriter, r *http.Request) {
	if !s.syncer.TryLock() {
		slog.Warn("sync already in progress, skipping")
		if isHtmx(r) {
			var buf bytes.Buffer
			if err := s.renderReposTbody(&buf, r); err != nil {
				slog.Error("render repos tbody", "error", err)
				http.Error(w, "Internal Server Error", http.StatusInternalServerError)
				return
			}
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			buf.WriteTo(w)
			return
		}
		http.Redirect(w, r, "/repos", http.StatusFound)
		return
	}
	defer s.syncer.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	result, err := s.syncer.SyncRepos(ctx)
	if err != nil {
		slog.Error("trigger sync",
			"error", err,
			"added", result.Added,
			"updated", result.Updated,
			"renamed", result.Renamed,
			"transferred", result.Transferred,
			"deleted", result.Deleted,
			"restored", result.Restored,
			"unchanged", result.Unchanged,
			"errors", result.ErrorCount,
		)
	}

	if isHtmx(r) {
		var buf bytes.Buffer
		if renderErr := s.renderReposTbody(&buf, r); renderErr != nil {
			slog.Error("render repos tbody after sync", "error", renderErr)
			http.Error(w, "Internal Server Error", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		if err != nil {
			w.Header().Set("HX-Trigger", `{"sync-failed":true}`)
		}
		buf.WriteTo(w)
		return
	}
	http.Redirect(w, r, "/repos", http.StatusFound)
}

func (s *Server) handleTriggerBackup(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("HX-Request") == "true" {
		w.Header().Set("HX-Redirect", "/")
	} else {
		http.Redirect(w, r, "/", http.StatusFound)
	}

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
			if repo.Format == model.FormatBundle {
				if err := s.engine.CloneRepo(ctx, repo); err != nil {
					slog.Error("backup repo", "owner", repo.Owner, "name", repo.Name, "error", err)
					s.createLogEntry(repo.ID, "backup", "error", err.Error())
					continue
				}
				if err := s.engine.CreateBundle(ctx, repo); err != nil {
					slog.Error("create bundle", "owner", repo.Owner, "name", repo.Name, "error", err)
					s.createLogEntry(repo.ID, "backup", "error", err.Error())
					continue
				}
			} else {
				if err := s.engine.CloneRepo(ctx, repo); err != nil {
					slog.Error("backup repo", "owner", repo.Owner, "name", repo.Name, "error", err)
					s.createLogEntry(repo.ID, "backup", "error", err.Error())
					continue
				}
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

type logView struct {
	model.LogEntry
	RepoOwner string
	RepoName  string
}

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

	views := make([]logView, 0, len(pageEntries))
	for _, e := range pageEntries {
		v := logView{LogEntry: e}
		v.CreatedAt = e.CreatedAt.Local()
		if e.RepoID != 0 {
			repo, err := s.repos.Get(e.RepoID)
			if err == nil {
				v.RepoOwner = repo.Owner
				v.RepoName = repo.Name
			}
		}
		views = append(views, v)
	}

	data := struct {
		Logs        []logView
		Page        int
		Size        int
		PrevPage    int
		NextPage    int
		HasNext     bool
		CSRFToken   string
		CurrentPath string
	}{
		Logs:        views,
		Page:        page,
		Size:        size,
		PrevPage:    page - 1,
		NextPage:    page + 1,
		HasNext:     len(entries) == total,
		CSRFToken:   s.csrfToken(r),
		CurrentPath: r.URL.Path,
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
		Settings    model.Settings
		Reason      string
		CSRFToken   string
		CurrentPath string
	}{
		Settings:    settings,
		Reason:      r.URL.Query().Get("reason"),
		CSRFToken:   s.csrfToken(r),
		CurrentPath: r.URL.Path,
	}

	s.renderTemplate(w, "settings", data)
}

func (s *Server) handleSettingsPost(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}

	cron := r.FormValue("cron_schedule")
	logRetentionDays := r.FormValue("log_retention_days")

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
