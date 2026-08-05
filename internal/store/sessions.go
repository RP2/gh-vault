package store

import (
	"crypto/rand"
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
	DeleteExpired() (int64, error)
}

var _ SessionStore = (*sessionsStore)(nil)

const sessionColumns = "id, user_id, csrf_token, expires_at, created_at"

const sessionInsertSQL = `INSERT INTO sessions (id, user_id, csrf_token, expires_at, created_at)
VALUES (?, ?, ?, ?, ?)`

const sessionGetSQL = "SELECT " + sessionColumns + " FROM sessions WHERE id = ?"

const sessionDeleteSQL = "DELETE FROM sessions WHERE id = ?"

const sessionListForExpirySQL = "SELECT id, expires_at FROM sessions"

func newRandomToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("rand read: %w", err)
	}
	return hex.EncodeToString(b), nil
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

	_, err = s.db.Exec(sessionInsertSQL, id, userID, csrf, expires, now)
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
	err := s.db.QueryRow(sessionGetSQL, id).Scan(
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
	_, err := s.db.Exec(sessionDeleteSQL, id)
	if err != nil {
		return fmt.Errorf("store: delete session: %w", err)
	}
	return nil
}

func (s *sessionsStore) DeleteExpired() (int64, error) {
	rows, err := s.db.Query(sessionListForExpirySQL)
	if err != nil {
		return 0, fmt.Errorf("store: list sessions for expiry: %w", err)
	}
	defer rows.Close()

	var expired []string
	now := time.Now().UTC()
	for rows.Next() {
		var id string
		var expires time.Time
		if err := rows.Scan(&id, &expires); err != nil {
			return 0, fmt.Errorf("store: scan session for expiry: %w", err)
		}
		if expires.Before(now) {
			expired = append(expired, id)
		}
	}
	if err := rows.Err(); err != nil {
		return 0, fmt.Errorf("store: iterate sessions for expiry: %w", err)
	}

	var deleted int64
	for _, id := range expired {
		result, err := s.db.Exec(sessionDeleteSQL, id)
		if err != nil {
			return deleted, fmt.Errorf("store: delete expired session: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return deleted, fmt.Errorf("store: rows affected delete expired session: %w", err)
		}
		deleted += n
	}
	return deleted, nil
}
