package store

import (
	"database/sql"
	"fmt"
	"time"

	"github.com/RP2/gh-vault/internal/model"
)

type LogStore interface {
	Create(entry model.LogEntry) error
	Recent(limit int) ([]model.LogEntry, error)
	DeleteOlderThan(before time.Time) (int64, error)
}

var _ LogStore = (*logsStore)(nil)

const logColumns = "id, repo_id, action, status, message, created_at"

const logInsertSQL = "INSERT INTO logs (repo_id, action, status, message) VALUES (?, ?, ?, ?)"

const logRecentSQL = "SELECT " + logColumns + " FROM logs ORDER BY created_at DESC LIMIT ?"

const logDeleteOlderSQL = "DELETE FROM logs WHERE created_at < ?"

func (s *logsStore) Create(entry model.LogEntry) error {
	_, err := s.db.Exec(logInsertSQL, entry.RepoID, entry.Action, entry.Status, entry.Message)
	if err != nil {
		return fmt.Errorf("store: create log: %w", err)
	}
	return nil
}

func (s *logsStore) Recent(limit int) ([]model.LogEntry, error) {
	if limit <= 0 {
		return nil, fmt.Errorf("store: recent logs: limit must be > 0: %d", limit)
	}
	rows, err := s.db.Query(logRecentSQL, limit)
	if err != nil {
		return nil, fmt.Errorf("store: recent logs: %w", err)
	}
	defer rows.Close()

	entries := make([]model.LogEntry, 0, limit)
	for rows.Next() {
		var e model.LogEntry
		var repoID sql.NullInt64
		var message sql.NullString
		if err := rows.Scan(&e.ID, &repoID, &e.Action, &e.Status, &message, &e.CreatedAt); err != nil {
			return nil, fmt.Errorf("store: scan log: %w", err)
		}
		if repoID.Valid {
			e.RepoID = repoID.Int64
		}
		if message.Valid {
			e.Message = message.String
		}
		entries = append(entries, e)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate logs: %w", err)
	}
	return entries, nil
}

func (s *logsStore) DeleteOlderThan(before time.Time) (int64, error) {
	result, err := s.db.Exec(logDeleteOlderSQL, before)
	if err != nil {
		return 0, fmt.Errorf("store: delete old logs: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("store: rows affected delete logs: %w", err)
	}
	return n, nil
}
