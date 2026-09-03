package store

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/RP2/gh-vault/internal/model"
)

type SessionStore interface {
	Create(userID int64, ttl time.Duration) (model.Session, error)
	Get(id string) (*model.Session, error)
	Delete(id string) error
	DeleteAllForUser(userID int64) error
	DeleteExpired() (int64, error)
}

var _ SessionStore = (*sessionsStore)(nil)

const sessionColumns = "id, user_id, csrf_token, expires_at, created_at"

const sessionInsertSQL = `INSERT INTO sessions (id, user_id, csrf_token, expires_at, created_at)
VALUES (?, ?, ?, ?, ?)`

const sessionGetSQL = "SELECT " + sessionColumns + " FROM sessions WHERE id = ?"

const sessionDeleteSQL = "DELETE FROM sessions WHERE id = ?"

const sessionDeleteAllForUserSQL = "DELETE FROM sessions WHERE user_id = ?"

const sessionDeleteExpiredSQL = "DELETE FROM sessions WHERE expires_at < ?"

func newRandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand read: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// hashSessionID returns the SHA-256 hex digest of a raw session ID. The digest
// is used as the primary key in the sessions table so that a compromised
// database does not reveal usable session identifiers.
func hashSessionID(id string) string {
	sum := sha256.Sum256([]byte(id))
	return hex.EncodeToString(sum[:])
}

func (s *sessionsStore) Create(userID int64, ttl time.Duration) (model.Session, error) {
	id, err := newRandomToken()
	if err != nil {
		return model.Session{}, fmt.Errorf("store: generate session id: %w", err)
	}
	csrf, err := newRandomToken()
	if err != nil {
		return model.Session{}, fmt.Errorf("store: generate csrf token: %w", err)
	}

	now := time.Now().UTC()
	expires := now.Add(ttl)

	_, err = s.db.Exec(sessionInsertSQL, hashSessionID(id), userID, csrf, expires, now)
	if err != nil {
		return model.Session{}, fmt.Errorf("store: create session: %w", err)
	}

	return model.Session{
		ID:        id,
		UserID:    userID,
		CSRFToken: csrf,
		ExpiresAt: expires,
		CreatedAt: now,
	}, nil
}

func (s *sessionsStore) Get(id string) (*model.Session, error) {
	var sess model.Session
	err := s.db.QueryRow(sessionGetSQL, hashSessionID(id)).Scan(
		&sess.ID, &sess.UserID, &sess.CSRFToken, &sess.ExpiresAt, &sess.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get session: %w", err)
	}
	if sess.ExpiresAt.Before(time.Now().UTC()) {
		return nil, ErrNotFound
	}
	return &sess, nil
}

func (s *sessionsStore) Delete(id string) error {
	_, err := s.db.Exec(sessionDeleteSQL, hashSessionID(id))
	if err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

func (s *sessionsStore) DeleteAllForUser(userID int64) error {
	_, err := s.db.Exec(sessionDeleteAllForUserSQL, userID)
	if err != nil {
		return fmt.Errorf("store: delete all sessions for user %d: %w", userID, err)
	}
	return nil
}

func (s *sessionsStore) DeleteExpired() (int64, error) {
	result, err := s.db.Exec(sessionDeleteExpiredSQL, time.Now().UTC())
	if err != nil {
		return 0, fmt.Errorf("store: delete expired sessions: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: rows affected delete expired sessions: %w", err)
	}
	return n, nil
}
