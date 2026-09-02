package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
)

const setupCSRFTokenCookie = "csrf_setup"

func generateCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("csrf: generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
}

func setupCSRFTokenFromRequest(r *http.Request) string {
	cookie, err := r.Cookie(setupCSRFTokenCookie)
	if err != nil {
		return ""
	}
	return cookie.Value
}

func (s *Server) setSetupCSRFCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     setupCSRFTokenCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   3600,
		Secure:   !s.cfg.DisableTLS,
	})
}

func (s *Server) clearSetupCSRFCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{
		Name:     setupCSRFTokenCookie,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   -1,
		Secure:   !s.cfg.DisableTLS,
	})
}

func validateCSRF(r *http.Request, token string) bool {
	formVal := r.FormValue("csrf")
	headerVal := r.Header.Get("X-CSRF-Token")
	tokenBytes := []byte(token)
	if subtle.ConstantTimeCompare([]byte(formVal), tokenBytes) == 1 {
		return true
	}
	if subtle.ConstantTimeCompare([]byte(headerVal), tokenBytes) == 1 {
		return true
	}
	return false
}
