package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net/http"
)

func generateCSRFToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("csrf: generate token: %w", err)
	}
	return hex.EncodeToString(b), nil
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
