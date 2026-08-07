package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/RP2/gh-vault/internal/model"
)

var ErrNotFound = errors.New("store: not found")

type RepoStore interface {
	List() ([]model.Repo, error)
	ListFiltered(query string) ([]model.Repo, error)
	ListActive() ([]model.Repo, error)
	ListDeleted() ([]model.Repo, error)
	Get(id int64) (model.Repo, error)
	GetByOwnerName(owner, name string) (model.Repo, error)
	Upsert(r model.Repo) (int64, error)
	SetLastBackup(id int64, at *time.Time) error
	SetGitHubMetadata(id int64, sizeKB int64, language, url *string, archived bool, private bool, lastPush *time.Time) error
	SetOwnerName(id int64, owner, name, url string) error
	SetBackupPath(id int64, path string) error
	MarkDeleted(id int64) error
	UnmarkDeleted(id int64) error
	Delete(id int64) error
	SetFormat(id int64, f model.RepoFormat, path string) error
	SetVerified(id int64, at *time.Time) error
	SetBackupEnabled(id int64, enabled bool) error
}

var _ RepoStore = (*reposStore)(nil)

const repoColumns = "id, github_id, owner, name, format, backup_path, github_url, language, size_kb, last_push, last_backup, verified_at, github_archived, github_deleted, private, backup_enabled, created_at, updated_at"

const repoUpsertSQL = `INSERT INTO repos (
	github_id, owner, name, format, backup_path, github_url, language, size_kb, last_push, github_archived, private, backup_enabled
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(github_id) DO UPDATE SET
	owner = excluded.owner,
	name = excluded.name,
	format = excluded.format,
	github_url = excluded.github_url,
	language = excluded.language,
	size_kb = excluded.size_kb,
	last_push = excluded.last_push,
	github_archived = excluded.github_archived,
	private = excluded.private,
	backup_enabled = excluded.backup_enabled
RETURNING id`

type rowScanner interface {
	Scan(dest ...any) error
}

func scanRepo(s rowScanner) (model.Repo, error) {
	var (
		r         model.Repo
		format    string
		backup    sql.NullString
		ghURL     sql.NullString
		language  sql.NullString
		lastPush  sql.NullTime
		lastBk    sql.NullTime
		verified  sql.NullTime
	)
	if err := s.Scan(
		&r.ID, &r.GitHubID, &r.Owner, &r.Name, &format,
		&backup, &ghURL, &language, &r.SizeKB,
		&lastPush, &lastBk, &verified,
		&r.GitHubArchived, &r.GitHubDeleted, &r.Private, &r.BackupEnabled,
		&r.CreatedAt, &r.UpdatedAt,
	); err != nil {
		return model.Repo{}, err
	}
	r.Format = model.RepoFormat(format)
	if backup.Valid {
		v := backup.String
		r.BackupPath = &v
	}
	if ghURL.Valid {
		v := ghURL.String
		r.GitHubURL = &v
	}
	if language.Valid {
		v := language.String
		r.Language = &v
	}
	if lastPush.Valid {
		t := lastPush.Time
		r.LastPush = &t
	}
	if lastBk.Valid {
		t := lastBk.Time
		r.LastBackup = &t
	}
	if verified.Valid {
		t := verified.Time
		r.VerifiedAt = &t
	}
	return r, nil
}

func nullStringValue(s *string) sql.NullString {
	if s == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: *s, Valid: true}
}

func nullTimeValue(t *time.Time) sql.NullTime {
	if t == nil {
		return sql.NullTime{}
	}
	return sql.NullTime{Time: *t, Valid: true}
}

func (s *reposStore) List() ([]model.Repo, error) {
	rows, err := s.db.Query("SELECT " + repoColumns + " FROM repos ORDER BY owner, name")
	if err != nil {
		return nil, fmt.Errorf("store: list repos: %w", err)
	}
	defer rows.Close()

	var repos []model.Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan repo: %w", err)
		}
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate repos: %w", err)
	}
	return repos, nil
}

func (s *reposStore) ListFiltered(query string) ([]model.Repo, error) {
	rows, err := s.db.Query("SELECT "+repoColumns+" FROM repos WHERE owner LIKE '%' || ? || '%' OR name LIKE '%' || ? || '%' ORDER BY owner, name", query, query)
	if err != nil {
		return nil, fmt.Errorf("store: list filtered repos: %w", err)
	}
	defer rows.Close()

	var repos []model.Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan repo: %w", err)
		}
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate repos: %w", err)
	}
	return repos, nil
}

func (s *reposStore) ListActive() ([]model.Repo, error) {
	rows, err := s.db.Query("SELECT " + repoColumns + " FROM repos WHERE github_deleted = 0 ORDER BY owner, name")
	if err != nil {
		return nil, fmt.Errorf("store: list active repos: %w", err)
	}
	defer rows.Close()

	var repos []model.Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan repo: %w", err)
		}
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate repos: %w", err)
	}
	return repos, nil
}

func (s *reposStore) ListDeleted() ([]model.Repo, error) {
	rows, err := s.db.Query("SELECT " + repoColumns + " FROM repos WHERE github_deleted = 1 ORDER BY owner, name")
	if err != nil {
		return nil, fmt.Errorf("store: list deleted repos: %w", err)
	}
	defer rows.Close()

	var repos []model.Repo
	for rows.Next() {
		r, err := scanRepo(rows)
		if err != nil {
			return nil, fmt.Errorf("store: scan repo: %w", err)
		}
		repos = append(repos, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store: iterate repos: %w", err)
	}
	return repos, nil
}

func (s *reposStore) Get(id int64) (model.Repo, error) {
	row := s.db.QueryRow("SELECT "+repoColumns+" FROM repos WHERE id = ?", id)
	r, err := scanRepo(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Repo{}, ErrNotFound
		}
		return model.Repo{}, fmt.Errorf("store: get repo %d: %w", id, err)
	}
	return r, nil
}

func (s *reposStore) GetByOwnerName(owner, name string) (model.Repo, error) {
	row := s.db.QueryRow("SELECT "+repoColumns+" FROM repos WHERE owner = ? AND name = ?", owner, name)
	r, err := scanRepo(row)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return model.Repo{}, ErrNotFound
		}
		return model.Repo{}, fmt.Errorf("store: get repo %s/%s: %w", owner, name, err)
	}
	return r, nil
}

func (s *reposStore) Upsert(r model.Repo) (int64, error) {
	var id int64
	err := s.db.QueryRow(
		repoUpsertSQL,
		r.GitHubID, r.Owner, r.Name, string(r.Format),
		nullStringValue(r.BackupPath),
		nullStringValue(r.GitHubURL),
		nullStringValue(r.Language),
		r.SizeKB,
		nullTimeValue(r.LastPush),
		r.GitHubArchived,
		r.Private,
		r.BackupEnabled,
	).Scan(&id)
	if err != nil {
		return 0, fmt.Errorf("store: upsert repo %s/%s: %w", r.Owner, r.Name, err)
	}
	return id, nil
}

func (s *reposStore) SetLastBackup(id int64, at *time.Time) error {
	result, err := s.db.Exec("UPDATE repos SET last_backup = ? WHERE id = ?", nullTimeValue(at), id)
	if err != nil {
		return fmt.Errorf("store: set last_backup for %d: %w", id, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *reposStore) SetGitHubMetadata(id int64, sizeKB int64, language, url *string, archived bool, private bool, lastPush *time.Time) error {
	result, err := s.db.Exec(
		"UPDATE repos SET size_kb = ?, language = ?, github_url = ?, github_archived = ?, private = ?, last_push = ? WHERE id = ?",
		sizeKB,
		nullStringValue(language),
		nullStringValue(url),
		archived,
		private,
		nullTimeValue(lastPush),
		id,
	)
	if err != nil {
		return fmt.Errorf("store: set github metadata for %d: %w", id, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *reposStore) SetOwnerName(id int64, owner, name, url string) error {
	result, err := s.db.Exec(
		"UPDATE repos SET owner = ?, name = ?, github_url = ? WHERE id = ?",
		owner, name, url, id,
	)
	if err != nil {
		return fmt.Errorf("store: set owner/name for %d: %w", id, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *reposStore) SetBackupPath(id int64, path string) error {
	result, err := s.db.Exec("UPDATE repos SET backup_path = ? WHERE id = ?", path, id)
	if err != nil {
		return fmt.Errorf("store: set backup_path for %d: %w", id, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *reposStore) MarkDeleted(id int64) error {
	result, err := s.db.Exec("UPDATE repos SET github_deleted = TRUE WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: mark deleted %d: %w", id, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *reposStore) UnmarkDeleted(id int64) error {
	result, err := s.db.Exec("UPDATE repos SET github_deleted = 0 WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: unmark deleted %d: %w", id, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *reposStore) Delete(id int64) error {
	result, err := s.db.Exec("DELETE FROM repos WHERE id = ?", id)
	if err != nil {
		return fmt.Errorf("store: delete repo %d: %w", id, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *reposStore) SetFormat(id int64, f model.RepoFormat, path string) error {
	result, err := s.db.Exec(
		"UPDATE repos SET format = ?, backup_path = ? WHERE id = ?",
		string(f), path, id,
	)
	if err != nil {
		return fmt.Errorf("store: set format for %d: %w", id, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *reposStore) SetVerified(id int64, at *time.Time) error {
	result, err := s.db.Exec(
		"UPDATE repos SET verified_at = ? WHERE id = ?",
		nullTimeValue(at), id,
	)
	if err != nil {
		return fmt.Errorf("store: set verified_at for %d: %w", id, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *reposStore) SetBackupEnabled(id int64, enabled bool) error {
	result, err := s.db.Exec("UPDATE repos SET backup_enabled = ? WHERE id = ?", enabled, id)
	if err != nil {
		return fmt.Errorf("store: set backup enabled %d: %w", id, err)
	}
	n, _ := result.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
