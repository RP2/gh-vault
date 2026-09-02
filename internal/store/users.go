package store

import (
	"database/sql"
	"errors"
	"fmt"

	"github.com/RP2/gh-vault/internal/model"
	"modernc.org/sqlite"
)

var (
	ErrUsernameExists = errors.New("store: username already exists")
	ErrSingleUserOnly = errors.New("store: single user only")
)

type UserStore interface {
	Count() (int, error)
	Create(username, passwordHash string) (int64, error)
	GetByID(id int64) (*model.User, error)
	GetByUsername(username string) (*model.User, error)
	UpdatePassword(userID int64, newHash string) error
	ChangePassword(userID int64, newHash string) error
}

var _ UserStore = (*usersStore)(nil)

const userColumns = "id, username, password_hash, created_at"

const userCountSQL = "SELECT COUNT(*) FROM users"

const userInsertSQL = `INSERT INTO users (username, password_hash) VALUES (?, ?) RETURNING id`

const userGetByUsernameSQL = "SELECT " + userColumns + " FROM users WHERE username = ?"

const userGetByIDSQL = "SELECT " + userColumns + " FROM users WHERE id = ?"

func (s *usersStore) GetByID(id int64) (*model.User, error) {
	var u model.User
	err := s.db.QueryRow(userGetByIDSQL, id).Scan(
		&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("store: get user %d: %w", id, err)
	}
	return &u, nil
}

func (s *usersStore) Count() (int, error) {
	var n int
	if err := s.db.QueryRow(userCountSQL).Scan(&n); err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}
	return n, nil
}

func (s *usersStore) Create(username, passwordHash string) (int64, error) {
	tx, err := s.db.Begin()
	if err != nil {
		return 0, fmt.Errorf("store: begin tx: %w", err)
	}
	defer tx.Rollback()

	var count int
	if err := tx.QueryRow(userCountSQL).Scan(&count); err != nil {
		return 0, fmt.Errorf("store: count users: %w", err)
	}
	if count > 0 {
		return 0, ErrSingleUserOnly
	}

	var id int64
	if err := tx.QueryRow(userInsertSQL, username, passwordHash).Scan(&id); err != nil {
		var sqliteErr *sqlite.Error
		if errors.As(err, &sqliteErr) && sqliteErr.Code() == 1555 { // SQLITE_CONSTRAINT_UNIQUE
			return 0, fmt.Errorf("store: create user %q: %w", username, ErrUsernameExists)
		}
		return 0, fmt.Errorf("store: create user %q: %w", username, err)
	}

	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("store: commit user creation: %w", err)
	}
	return id, nil
}

const userUpdatePasswordSQL = "UPDATE users SET password_hash = ? WHERE id = ?"

const userChangePasswordDeleteSessionsSQL = "DELETE FROM sessions WHERE user_id = ?"

func (s *usersStore) UpdatePassword(userID int64, newHash string) error {
	result, err := s.db.Exec(userUpdatePasswordSQL, newHash, userID)
	if err != nil {
		return fmt.Errorf("store: update password for %d: %w", userID, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ChangePassword updates the user's password hash and invalidates all of the
// user's existing sessions in a single transaction.
func (s *usersStore) ChangePassword(userID int64, newHash string) error {
	tx, err := s.db.Begin()
	if err != nil {
		return fmt.Errorf("store: begin change password tx: %w", err)
	}
	defer tx.Rollback()

	result, err := tx.Exec(userUpdatePasswordSQL, newHash, userID)
	if err != nil {
		return fmt.Errorf("store: update password for %d: %w", userID, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("store: rows affected updating password for %d: %w", userID, err)
	}
	if n == 0 {
		return ErrNotFound
	}

	if _, err := tx.Exec(userChangePasswordDeleteSessionsSQL, userID); err != nil {
		return fmt.Errorf("store: delete sessions for %d: %w", userID, err)
	}

	if err := tx.Commit(); err != nil {
		return fmt.Errorf("store: commit change password: %w", err)
	}
	return nil
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
