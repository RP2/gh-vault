package store

import (
	"database/sql"
	"fmt"
	"net/url"

	_ "modernc.org/sqlite"
)

type DB struct {
	db *sql.DB
}

func New(dbPath string) (*DB, error) {
	// synchronous(NORMAL) is the standard WAL-mode setting: commits no longer
	// fsync the journal on every transaction. Durability on power loss is
	// limited to the last checkpoint, which is acceptable for backup metadata.
	dsn := fmt.Sprintf("file:%s?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)", url.QueryEscape(dbPath))
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.Ping(); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: ping sqlite: %w", err)
	}
	if err := runMigrations(db); err != nil {
		db.Close()
		return nil, fmt.Errorf("store: migrate: %w", err)
	}
	return &DB{db: db}, nil
}

func (d *DB) Repos() *reposStore {
	return &reposStore{db: d.db}
}

func (d *DB) Logs() *logsStore {
	return &logsStore{db: d.db}
}

func (d *DB) Sessions() *sessionsStore {
	return &sessionsStore{db: d.db}
}

func (d *DB) Settings() *settingsStore {
	return &settingsStore{db: d.db}
}

func (d *DB) Users() *usersStore {
	return &usersStore{db: d.db}
}

func (d *DB) Secrets() *secretsStore {
	return &secretsStore{db: d.db}
}

func (d *DB) Close() error {
	return d.db.Close()
}

type reposStore struct {
	db *sql.DB
}

type logsStore struct {
	db *sql.DB
}

type sessionsStore struct {
	db *sql.DB
}

type settingsStore struct {
	db *sql.DB
}

type usersStore struct {
	db *sql.DB
}

type secretsStore struct {
	db *sql.DB
}
