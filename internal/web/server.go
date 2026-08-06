package web

import (
	"context"
	"embed"
	"fmt"
	"html/template"
	"mime"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/RP2/gh-vault/internal/backup"
	"github.com/RP2/gh-vault/internal/config"
	"github.com/RP2/gh-vault/internal/github"
	"github.com/RP2/gh-vault/internal/model"
	"github.com/RP2/gh-vault/internal/scheduler"
	"github.com/RP2/gh-vault/internal/store"
	reposync "github.com/RP2/gh-vault/internal/sync"
)

//go:embed all:templates
var templateFS embed.FS

//go:embed all:static
var staticFS embed.FS

type Server struct {
	cfg           *config.Config
	router        *chi.Mux
	templates     map[string]*template.Template
	users         store.UserStore
	sessions      store.SessionStore
	settings      store.SettingsStore
	repos         store.RepoStore
	logs          store.LogStore
	secrets       store.SecretStore
	engine        backup.Engine
	syncer        reposync.Syncer
	sched         scheduler.Scheduler
	tokenProvider github.TokenProvider
	ghClient      github.Client
	setupDone     bool
}

func NewServer(
	cfg *config.Config,
	users store.UserStore,
	settings store.SettingsStore,
	repos store.RepoStore,
	logs store.LogStore,
	secrets store.SecretStore,
	sessions store.SessionStore,
	engine backup.Engine,
	syncer reposync.Syncer,
	sched scheduler.Scheduler,
	tokenProvider github.TokenProvider,
	ghClient github.Client,
) (*Server, error) {
	layoutTmpl, err := template.ParseFS(templateFS, "templates/layout.html")
	if err != nil {
		return nil, fmt.Errorf("web: parse layout template: %w", err)
	}

	pages := []string{"dashboard", "repos", "logs", "settings", "login", "setup"}
	templates := make(map[string]*template.Template)
	for _, name := range pages {
		t, err := layoutTmpl.Clone()
		if err != nil {
			return nil, fmt.Errorf("web: clone layout: %w", err)
		}
		if _, err := t.ParseFS(templateFS, "templates/"+name+".html"); err != nil {
			return nil, fmt.Errorf("web: parse %s: %w", name, err)
		}
		templates[name] = t
	}

	count, err := users.Count()
	if err != nil {
		return nil, fmt.Errorf("web: count users: %w", err)
	}

	s := &Server{
		cfg:           cfg,
		users:         users,
		sessions:      sessions,
		settings:      settings,
		repos:         repos,
		logs:          logs,
		secrets:       secrets,
		engine:        engine,
		syncer:        syncer,
		sched:         sched,
		tokenProvider: tokenProvider,
		ghClient:      ghClient,
		templates:     templates,
		setupDone:     count > 0,
	}
	s.router = s.setupRouter()
	return s, nil
}

func (s *Server) setupRouter() *chi.Mux {
	r := chi.NewRouter()

	// Middleware stack (outermost first)
	r.Use(loggingMiddleware)
	r.Use(securityHeaders)
	r.Use(s.sessionMiddleware)
	r.Use(s.stateMachine)
	r.Use(s.csrfMiddleware)

	// Static files
	r.Handle("/static/*", s.staticFileHandler())

	// Health check (no auth)
	r.Get("/healthz", s.handleHealthz)

	// Setup wizard
	r.Get("/setup", s.handleSetupGet)
	r.Post("/setup", s.handleSetupPost)

	// Auth
	r.Get("/login", s.handleLoginGet)
	r.Post("/login", s.handleLoginPost)
	r.Post("/logout", s.handleLogout)

	// Dashboard
	r.Get("/", s.handleDashboard)

	// Repos
	r.Get("/repos", s.handleReposList)
	r.Post("/repos/{id}/switch", s.handleRepoSwitch)
	r.Post("/repos/{id}/archive", s.handleRepoArchive)
	r.Post("/repos/{id}/backup", s.handleRepoBackup)
	r.Post("/repos/{id}/backup-toggle", s.handleRepoBackupToggle)
	r.Post("/repos/{id}/auto-archive", s.handleRepoAutoArchive)
	r.Post("/backup-checked", s.handleBackupChecked)
	r.Post("/archive-checked", s.handleArchiveChecked)

	// Triggers
	r.Post("/sync", s.handleTriggerSync)
	r.Get("/sync/status", s.handleSyncStatus)
	r.Post("/backup-all", s.handleTriggerBackup)

	// Logs
	r.Get("/logs", s.handleLogs)

	// Settings
	r.Get("/settings", s.handleSettingsGet)
	r.Post("/settings", s.handleSettingsPost)
	r.Post("/settings/token", s.handleSettingsTokenPost)
	r.Get("/settings/token-status", s.handleSettingsTokenStatus)

	return r
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.router.ServeHTTP(w, r)
}

func (s *Server) hasToken(ctx context.Context) bool {
	token, err := s.tokenProvider.GetToken(ctx)
	return err == nil && token != ""
}

func (s *Server) getSession(r *http.Request) *model.Session {
	return sessionFromContext(r)
}

func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write([]byte(`{"status":"ok"}`))
}

func (s *Server) staticFileHandler() http.Handler {
	fs := http.FS(staticFS)
	fileServer := http.FileServer(fs)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/") {
			http.NotFound(w, r)
			return
		}
		ext := filepath.Ext(r.URL.Path)
		mw := &mimeResponseWriter{ResponseWriter: w, ext: ext}
		fileServer.ServeHTTP(mw, r)
	})
}

// mimeResponseWriter wraps ResponseWriter to override Content-Type from file extension.
// embed.FS + http.FileServer content-sniffs as text/plain; this fixes it.
type mimeResponseWriter struct {
	http.ResponseWriter
	ext string
}

func (w *mimeResponseWriter) WriteHeader(code int) {
	if ct := mime.TypeByExtension(w.ext); ct != "" {
		w.ResponseWriter.Header().Set("Content-Type", ct)
	}
	w.ResponseWriter.WriteHeader(code)
}

