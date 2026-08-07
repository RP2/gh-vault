package web

import (
	"context"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

// stateMachine enforces the routing state machine from PLAN.md
func (s *Server) stateMachine(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip middleware for these paths
		path := r.URL.Path
		if path == "/healthz" || path == "/login" || path == "/setup" ||
			strings.HasPrefix(path, "/static/") || path == "/logout" {
			next.ServeHTTP(w, r)
			return
		}

		// Step 1: No users → redirect to /setup
		if !s.setupDone && path != "/setup" {
			http.Redirect(w, r, "/setup", http.StatusFound)
			return
		}

		// Step 1b: Users exist, path is /setup → 404
		if s.setupDone && path == "/setup" {
			http.NotFound(w, r)
			return
		}

		// Step 2: No valid session → redirect to /login
		session := s.getSession(r)
		if session == nil && path != "/login" {
			http.Redirect(w, r, "/login", http.StatusFound)
			return
		}

		// Step 3: Valid session but no token → redirect to /settings
		if session != nil {
			hasToken := s.hasToken(r.Context())
		if !hasToken && path != "/settings" && path != "/settings/token" &&
			path != "/settings/token-status" && path != "/sync" &&
			path != "/backup-all" && path != "/logout" {
				http.Redirect(w, r, "/settings?reason=token_missing", http.StatusFound)
				return
			}
		}

		next.ServeHTTP(w, r)
	})
}

// noCacheMiddleware sets headers that prevent browsers from caching responses.
// Auth-protected pages must not be served from cache after logout, since the
// back/forward buttons would otherwise display authenticated content to a
// signed-out user.
func noCacheMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		next.ServeHTTP(w, r)
	})
}

// sessionMiddleware reads cookie, loads session, injects into context
func (s *Server) sessionMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cookie, err := r.Cookie("session")
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		session, err := s.sessions.Get(cookie.Value)
		if err != nil {
			next.ServeHTTP(w, r)
			return
		}
		ctx := context.WithValue(r.Context(), sessionContextKey, session)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// csrfMiddleware validates CSRF token on all POST requests
func (s *Server) csrfMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodGet, http.MethodHead, http.MethodOptions:
			next.ServeHTTP(w, r)
			return
		}
		// Skip CSRF for login, setup, healthz
		if r.URL.Path == "/login" || r.URL.Path == "/setup" || r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		session := sessionFromContext(r)
		if session == nil {
			next.ServeHTTP(w, r)
			return
		}
		if !validateCSRF(r, session.CSRFToken) {
			http.Error(w, "Forbidden: invalid CSRF token", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders adds standard security headers
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'")
		w.Header().Set("Referrer-Policy", "strict-origin-when-cross-origin")
		w.Header().Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		next.ServeHTTP(w, r)
	})
}

// statusRecorder wraps a ResponseWriter to capture the status code.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

// loggingMiddleware logs each request
func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(rec, r)
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "status", rec.status, "duration", time.Since(start))
	})
}
