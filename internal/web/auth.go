package web

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/RP2/gh-vault/internal/model"
)

type contextKey string

const sessionContextKey contextKey = "session"

// createSession creates a session, sets cookie, returns session
func (s *Server) createSession(w http.ResponseWriter, userID int64) error {
	session, err := s.sessions.Create(userID, 24*time.Hour)
	if err != nil {
		return fmt.Errorf("web: create session: %w", err)
	}
	cookie := &http.Cookie{
		Name:     "session",
		Value:    session.ID,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   86400,
	}
	if strings.HasPrefix(s.cfg.BaseURL, "https") {
		cookie.Secure = true
	}
	http.SetCookie(w, cookie)
	return nil
}

// destroySession deletes session and clears cookie
func (s *Server) destroySession(w http.ResponseWriter, r *http.Request) error {
	cookie, err := r.Cookie("session")
	if err != nil {
		return nil
	}
	if err := s.sessions.Delete(cookie.Value); err != nil {
		return fmt.Errorf("web: delete session: %w", err)
	}
	clearCookie := &http.Cookie{
		Name:     "session",
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
	}
	if strings.HasPrefix(s.cfg.BaseURL, "https") {
		clearCookie.Secure = true
	}
	http.SetCookie(w, clearCookie)
	return nil
}

// sessionFromContext retrieves the session from context
func sessionFromContext(r *http.Request) *model.Session {
	s, _ := r.Context().Value(sessionContextKey).(*model.Session)
	return s
}
