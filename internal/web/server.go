package web

import (
	"context"
	"embed"
	"html/template"
	"log/slog"
	"net/http"

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
	tmpl          *template.Template
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

	userCount cachedUserCount
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
) *Server {
	tmpl, err := template.ParseFS(templateFS, "templates/*.html")
	if err != nil {
		slog.Error("parse templates", "error", err)
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
		tmpl:          tmpl,
	}
	s.router = s.setupRouter()
	return s
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
	r.Handle("/static/*", http.StripPrefix("/static/", http.FileServer(http.FS(staticFS))))

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
	r.Post("/repos/{id}/delete", s.handleRepoDelete)
	r.Delete("/repos/{id}", s.handleRepoDeletePermanent)
	r.Post("/repos/{id}/backup", s.handleRepoBackup)
	r.Post("/repos/{id}/auto-archive", s.handleRepoAutoArchive)

	// Triggers
	r.Post("/trigger/sync", s.handleTriggerSync)
	r.Post("/trigger/backup", s.handleTriggerBackup)

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

