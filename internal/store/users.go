package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/RP2/gh-vault/internal/model"
	"modernc.org/sqlite"
)

var ErrUsernameExists = errors.New("store: username already exists")

type UserStore interface {
	Count() (int, error)
	Create(username, passwordHash string) (int64, error)
	GetByUsername(username string) (*model.User, error)
}

var _ UserStore = (*usersStore)(nil)

const userColumns = "id, username, password_hash, created_at"

const userCountSQL = "SELECT COUNT(*) FROM users"

const userInsertSQL = `INSERT INTO users (username, password_hash) VALUES (?, ?) RETURNING id`

const userGetByUsernameSQL = "SELECT " + userColumns + " FROM users WHERE username = ?"

func (s *usersStore) Count() (int, error) {
	var n int
	if err := s.db.QueryRow(userCountSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}
	return n, nil
}

func (s *usersStore) Create(username, passwordHash string) (int64, error) {
	var id int64
	err := s.db.QueryRow(userInsertSQL, username, passwordHash).Scan(&id)
	if err != nil {
		var sqliteErr *sqlite.Error
		if errors.As(err, &sqliteErr) && sqliteErr.Code() == 1555 { // SQLITE_CONSTRAINT_UNIQUE
			return 0, fmt.Errorf("store: create user %q: %w", username, ErrUsernameExists)
		}
		return 0, fmt.Errorf("store: create user %q: %w", username, err)
	}
	return id, nil
}

func (s *usersStore) GetByUsername(username string) (*model.User, error) {
	var u model.User
	err := s.db.QueryRow(userGetByUsernameSQL, username).Scan(
		&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get user %q: %w", username, err)
	}
	return &u, nil
}
