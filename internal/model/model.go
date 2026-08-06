package model

import "time"

// Repo represents a GitHub repository tracked by gh-vault.
type Repo struct {
	ID             int64      `db:"id" json:"id"`
	GitHubID       int64      `db:"github_id" json:"github_id"`
	Owner          string     `db:"owner" json:"owner"`
	Name           string     `db:"name" json:"name"`
	Format         RepoFormat `db:"format" json:"format"`
	BackupPath     *string    `db:"backup_path" json:"backup_path"`
	GitHubURL      *string    `db:"github_url" json:"github_url"`
	Language       *string    `db:"language" json:"language"`
	SizeKB         int64      `db:"size_kb" json:"size_kb"`
	LastPush       *time.Time `db:"last_push" json:"last_push"`
	LastBackup     *time.Time `db:"last_backup" json:"last_backup"`
	VerifiedAt     *time.Time `db:"verified_at" json:"verified_at"`
	GitHubArchived bool       `db:"github_archived" json:"github_archived"`
	GitHubDeleted  bool       `db:"github_deleted" json:"github_deleted"`
	AutoArchive    bool       `db:"auto_archive" json:"auto_archive"`
	BackupEnabled  bool       `db:"backup_enabled" json:"backup_enabled"`
	CreatedAt      time.Time  `db:"created_at" json:"created_at"`
	UpdatedAt      time.Time  `db:"updated_at" json:"updated_at"`
}

// RepoFormat defines the storage format for a repository backup.
type RepoFormat string

const (
	FormatClone  RepoFormat = "clone"
	FormatBundle RepoFormat = "bundle"
)

// LogEntry records an action performed on a repository.
type LogEntry struct {
	ID        int64     `db:"id" json:"id"`
	RepoID    int64     `db:"repo_id" json:"repo_id"`
	Action    string    `db:"action" json:"action"`
	Status    string    `db:"status" json:"status"`
	Message   string    `db:"message" json:"message"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// Session represents an authenticated user session.
type Session struct {
	ID        string    `db:"id" json:"id"`
	UserID    int64     `db:"user_id" json:"user_id"`
	CSRFToken string    `db:"csrf_token" json:"csrf_token"`
	ExpiresAt time.Time `db:"expires_at" json:"expires_at"`
	CreatedAt time.Time `db:"created_at" json:"created_at"`
}

// User represents an authenticated user of gh-vault.
type User struct {
	ID           int64     `db:"id" json:"id"`
	Username     string    `db:"username" json:"username"`
	PasswordHash string    `db:"password_hash" json:"password_hash"`
	CreatedAt    time.Time `db:"created_at" json:"created_at"`
}

// SettingsStore.GetAll() parses each key to its typed field. Unknown keys ignored.
// Settings holds application-wide configuration values.
type Settings struct {
	CronSchedule     string `db:"cron_schedule" json:"cron_schedule"`
	DryRun           bool   `db:"dry_run" json:"dry_run"`
	AutoArchiveDays  int    `db:"auto_archive_days" json:"auto_archive_days"`
	LogRetentionDays int    `db:"log_retention_days" json:"log_retention_days"`
}
