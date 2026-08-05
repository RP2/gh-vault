package store

import (
	"database/sql"
	"fmt"
)

const v1DDL = `
CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS repos (
    id              INTEGER PRIMARY KEY,
    github_id       INTEGER NOT NULL UNIQUE,
    owner           TEXT NOT NULL,
    name            TEXT NOT NULL,
    format          TEXT NOT NULL DEFAULT 'clone' CHECK(format IN ('clone', 'bundle')),
    backup_path     TEXT,
    github_url      TEXT,
    language        TEXT,
    size_kb         INTEGER DEFAULT 0,
    last_push       DATETIME,
    last_backup     DATETIME,
    verified_at     DATETIME,
    github_archived BOOLEAN NOT NULL DEFAULT FALSE,
    github_deleted  BOOLEAN NOT NULL DEFAULT FALSE,
    auto_archive    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(owner, name)
);

CREATE TRIGGER IF NOT EXISTS trg_repos_updated_at AFTER UPDATE ON repos
BEGIN UPDATE repos SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;

CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    user_id    INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_token TEXT NOT NULL,
    expires_at DATETIME NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS logs (
    id         INTEGER PRIMARY KEY,
    repo_id    INTEGER REFERENCES repos(id) ON DELETE CASCADE,
    action     TEXT NOT NULL,
    status     TEXT NOT NULL CHECK(status IN ('success', 'error')),
    message    TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_logs_created_at ON logs(created_at DESC);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

INSERT OR IGNORE INTO settings(key, value) VALUES ('cron_schedule', '0 3 1 * *');
INSERT OR IGNORE INTO settings(key, value) VALUES ('dry_run', 'false');
INSERT OR IGNORE INTO settings(key, value) VALUES ('auto_archive_days', '0');
INSERT OR IGNORE INTO settings(key, value) VALUES ('log_retention_days', '90');
`

const v2DDL = `
CREATE TABLE IF NOT EXISTS secrets (
    key        TEXT PRIMARY KEY,
    value      BLOB NOT NULL,
    nonce      BLOB NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);
`

type migration struct {
	Version int
	DDL     string
}

var migrations = []migration{
	{Version: 1, DDL: v1DDL},
	{Version: 2, DDL: v2DDL},
}

func runMigrations(db *sql.DB) error {
	var currentVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&currentVersion); err != nil {
		return fmt.Errorf("store: read user_version: %w", err)
	}
	for _, m := range migrations {
		if m.Version <= currentVersion {
			continue
		}
		if err := applyMigration(db, m); err != nil {
			return fmt.Errorf("store: apply migration v%d: %w", m.Version, err)
		}
	}
	return nil
}

func applyMigration(db *sql.DB, m migration) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("begin transaction: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.Exec(m.DDL); err != nil {
		return fmt.Errorf("exec DDL: %w", err)
	}
	if _, err := tx.Exec(fmt.Sprintf("PRAGMA user_version = %d", m.Version)); err != nil {
		return fmt.Errorf("set user_version: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}
