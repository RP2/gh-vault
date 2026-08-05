package store

import (
	"database/sql"
	"errors"
	"fmt"
)

type SecretStore interface {
	Get(key string) (value []byte, nonce []byte, err error)
	Set(key string, value []byte, nonce []byte) error
	Delete(key string) error
}

var _ SecretStore = (*secretsStore)(nil)

const secretGetSQL = "SELECT value, nonce FROM secrets WHERE key = ?"

const secretUpsertSQL = `INSERT INTO secrets (key, value, nonce) VALUES (?, ?, ?)
ON CONFLICT(key) DO UPDATE SET
	value = excluded.value,
	nonce = excluded.nonce,
	updated_at = CURRENT_TIMESTAMP`

const secretDeleteSQL = "DELETE FROM secrets WHERE key = ?"

func (s *secretsStore) Get(key string) ([]byte, []byte, error) {
	var value, nonce []byte
	err := s.db.QueryRow(secretGetSQL, key).Scan(&value, &nonce)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil, ErrNotFound
		}
		return nil, nil, fmt.Errorf("store: get secret %q: %w", key, err)
	}
	return value, nonce, nil
}

func (s *secretsStore) Set(key string, value []byte, nonce []byte) error {
	_, err := s.db.Exec(secretUpsertSQL, key, value, nonce)
	if err != nil {
		return fmt.Errorf("store: set secret %q: %w", key, err)
	}
	return nil
}

func (s *secretsStore) Delete(key string) error {
	result, err := s.db.Exec(secretDeleteSQL, key)
	if err != nil {
		return fmt.Errorf("store: delete secret %q: %w", key, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rows affected delete secret %q: %w", key, err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
