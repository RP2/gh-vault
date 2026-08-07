package store

import (
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/RP2/gh-vault/internal/model"
)

func ptr[T any](v T) *T {
	return &v
}

func openTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func insertTestRepo(t *testing.T, db *DB, githubID int64, owner, name string) int64 {
	t.Helper()
	id, err := db.Repos().Upsert(model.Repo{
		GitHubID: githubID,
		Owner:    owner,
		Name:     name,
		Format:   model.FormatClone,
	})
	if err != nil {
		t.Fatalf("insert test repo: %v", err)
	}
	return id
}

func TestSettingsValidateCron(t *testing.T) {
	db := openTestDB(t)

	if err := db.Settings().Set("cron_schedule", "0 3 1 * *"); err != nil {
		t.Errorf("valid cron schedule: expected nil error, got %v", err)
	}
	if err := db.Settings().Set("cron_schedule", "invalid"); err == nil {
		t.Errorf("invalid cron schedule: expected error, got nil")
	}
	if err := db.Settings().Set("cron_schedule", ""); err == nil {
		t.Errorf("empty cron schedule: expected error, got nil")
	}
}

func TestSettingsValidateRetention(t *testing.T) {
	db := openTestDB(t)

	cases := []struct {
		value string
		valid bool
	}{
		{"90", true},
		{"0", true},
		{"36500", true},
		{"36501", false},
		{"-1", false},
		{"99999", false},
		{"abc", false},
	}

	for _, tc := range cases {
		err := db.Settings().Set("log_retention_days", tc.value)
		if tc.valid && err != nil {
			t.Errorf("valid retention days %q: expected nil error, got %v", tc.value, err)
		}
		if !tc.valid && err == nil {
			t.Errorf("invalid retention days %q: expected error, got nil", tc.value)
		}
	}
}

func TestSettingsSetAndGet(t *testing.T) {
	db := openTestDB(t)

	if err := db.Settings().Set("cron_schedule", "0 4 1 * *"); err != nil {
		t.Fatalf("set cron schedule: %v", err)
	}
	got, err := db.Settings().Get("cron_schedule")
	if err != nil {
		t.Fatalf("get cron schedule: %v", err)
	}
	if got != "0 4 1 * *" {
		t.Errorf("cron schedule = %q, want %q", got, "0 4 1 * *")
	}

	if err := db.Settings().Set("cron_schedule", "0 5 1 * *"); err != nil {
		t.Fatalf("overwrite cron schedule: %v", err)
	}
	got, err = db.Settings().Get("cron_schedule")
	if err != nil {
		t.Fatalf("get overwritten cron schedule: %v", err)
	}
	if got != "0 5 1 * *" {
		t.Errorf("overwritten cron schedule = %q, want %q", got, "0 5 1 * *")
	}

	if err := db.Settings().Set("cron_schedule", "invalid"); err == nil {
		t.Fatalf("invalid cron schedule: expected error, got nil")
	}
	got, err = db.Settings().Get("cron_schedule")
	if err != nil {
		t.Fatalf("get cron schedule after invalid set: %v", err)
	}
	if got != "0 5 1 * *" {
		t.Errorf("cron schedule after invalid set = %q, want %q", got, "0 5 1 * *")
	}
}

func TestSettingsGetMissing(t *testing.T) {
	db := openTestDB(t)

	_, err := db.Settings().Get("nonexistent")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(nonexistent) error = %v, want ErrNotFound", err)
	}
}

func TestSettingsGetAll(t *testing.T) {
	db := openTestDB(t)

	if err := db.Settings().Set("cron_schedule", "0 6 1 * *"); err != nil {
		t.Fatalf("set cron schedule: %v", err)
	}
	if err := db.Settings().Set("log_retention_days", "30"); err != nil {
		t.Fatalf("set log retention days: %v", err)
	}

	settings, err := db.Settings().GetAll()
	if err != nil {
		t.Fatalf("get all settings: %v", err)
	}
	if settings.CronSchedule != "0 6 1 * *" {
		t.Errorf("CronSchedule = %q, want %q", settings.CronSchedule, "0 6 1 * *")
	}
	if settings.LogRetentionDays != 30 {
		t.Errorf("LogRetentionDays = %d, want %d", settings.LogRetentionDays, 30)
	}
}

func TestSettingsGetAllDefaults(t *testing.T) {
	db := openTestDB(t)

	settings, err := db.Settings().GetAll()
	if err != nil {
		t.Fatalf("get all settings: %v", err)
	}
	if settings.CronSchedule != "0 3 1 * *" {
		t.Errorf("CronSchedule = %q, want %q", settings.CronSchedule, "0 3 1 * *")
	}
	if settings.LogRetentionDays != 90 {
		t.Errorf("LogRetentionDays = %d, want %d", settings.LogRetentionDays, 90)
	}
}

func TestSettingsGetAllInvalidValue(t *testing.T) {
	db := openTestDB(t)

	if _, err := db.db.Exec("INSERT OR REPLACE INTO settings(key, value) VALUES ('log_retention_days', 'not-a-number')"); err != nil {
		t.Fatalf("insert invalid setting: %v", err)
	}

	settings, err := db.Settings().GetAll()
	if err != nil {
		t.Fatalf("get all settings: %v", err)
	}
	if settings.LogRetentionDays != 0 {
		t.Errorf("LogRetentionDays = %d, want 0", settings.LogRetentionDays)
	}
}

func TestReposUpsertAndGet(t *testing.T) {
	db := openTestDB(t)

	now := time.Now().Truncate(time.Second)
	repoIn := model.Repo{
		GitHubID:       1,
		Owner:          "RP2",
		Name:           "gh-vault",
		Format:         model.FormatClone,
		BackupPath:     ptr("active/RP2/gh-vault.git"),
		GitHubURL:      ptr("https://github.com/RP2/gh-vault"),
		Language:       ptr("Go"),
		SizeKB:         100,
		LastPush:       &now,
		GitHubArchived: false,
		Private:        true,
		BackupEnabled:  true,
	}

	id, err := db.Repos().Upsert(repoIn)
	if err != nil {
		t.Fatalf("upsert repo: %v", err)
	}

	got, err := db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}

	if got.ID != id {
		t.Errorf("ID = %d, want %d", got.ID, id)
	}
	if got.GitHubID != repoIn.GitHubID {
		t.Errorf("GitHubID = %d, want %d", got.GitHubID, repoIn.GitHubID)
	}
	if got.Owner != repoIn.Owner {
		t.Errorf("Owner = %q, want %q", got.Owner, repoIn.Owner)
	}
	if got.Name != repoIn.Name {
		t.Errorf("Name = %q, want %q", got.Name, repoIn.Name)
	}
	if got.Format != repoIn.Format {
		t.Errorf("Format = %q, want %q", got.Format, repoIn.Format)
	}
	if got.BackupPath == nil || *got.BackupPath != *repoIn.BackupPath {
		t.Errorf("BackupPath = %v, want %v", got.BackupPath, repoIn.BackupPath)
	}
	if got.GitHubURL == nil || *got.GitHubURL != *repoIn.GitHubURL {
		t.Errorf("GitHubURL = %v, want %v", got.GitHubURL, repoIn.GitHubURL)
	}
	if got.Language == nil || *got.Language != *repoIn.Language {
		t.Errorf("Language = %v, want %v", got.Language, repoIn.Language)
	}
	if got.SizeKB != repoIn.SizeKB {
		t.Errorf("SizeKB = %d, want %d", got.SizeKB, repoIn.SizeKB)
	}
	if got.LastPush == nil || !got.LastPush.Equal(*repoIn.LastPush) {
		t.Errorf("LastPush = %v, want %v", got.LastPush, repoIn.LastPush)
	}
	if got.GitHubArchived != repoIn.GitHubArchived {
		t.Errorf("GitHubArchived = %v, want %v", got.GitHubArchived, repoIn.GitHubArchived)
	}
	if got.Private != repoIn.Private {
		t.Errorf("Private = %v, want %v", got.Private, repoIn.Private)
	}
	if got.BackupEnabled != repoIn.BackupEnabled {
		t.Errorf("BackupEnabled = %v, want %v", got.BackupEnabled, repoIn.BackupEnabled)
	}
	if got.CreatedAt.IsZero() {
		t.Errorf("CreatedAt is zero")
	}
	if got.UpdatedAt.IsZero() {
		t.Errorf("UpdatedAt is zero")
	}
}

func TestReposUpsertUpdate(t *testing.T) {
	db := openTestDB(t)

	id, err := db.Repos().Upsert(model.Repo{
		GitHubID: 1,
		Owner:    "old",
		Name:     "repo",
		Format:   model.FormatClone,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	_, err = db.Repos().Upsert(model.Repo{
		GitHubID: 1,
		Owner:    "new",
		Name:     "repo",
		Format:   model.FormatClone,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.Owner != "new" {
		t.Errorf("Owner = %q, want %q", got.Owner, "new")
	}
}

func TestReposUpsertBackupEnabled(t *testing.T) {
	db := openTestDB(t)

	id, err := db.Repos().Upsert(model.Repo{
		GitHubID:      1,
		Owner:         "owner",
		Name:          "repo",
		Format:        model.FormatClone,
		BackupEnabled: true,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	_, err = db.Repos().Upsert(model.Repo{
		GitHubID:      1,
		Owner:         "owner",
		Name:          "repo",
		Format:        model.FormatClone,
		BackupEnabled: false,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}

	got, err := db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.BackupEnabled {
		t.Errorf("BackupEnabled = %v, want false", got.BackupEnabled)
	}
}

func TestReposList(t *testing.T) {
	db := openTestDB(t)

	insertTestRepo(t, db, 1, "A", "zeta")
	insertTestRepo(t, db, 2, "Z", "alpha")

	repos, err := db.Repos().List()
	if err != nil {
		t.Fatalf("list repos: %v", err)
	}
	if len(repos) != 2 {
		t.Fatalf("len(repos) = %d, want 2", len(repos))
	}
	want := []struct {
		owner, name string
	}{
		{"A", "zeta"},
		{"Z", "alpha"},
	}
	for i, r := range repos {
		if r.Owner != want[i].owner || r.Name != want[i].name {
			t.Errorf("repos[%d] = %q/%q, want %q/%q", i, r.Owner, r.Name, want[i].owner, want[i].name)
		}
	}
}

func TestReposListEmpty(t *testing.T) {
	db := openTestDB(t)

	repos, err := db.Repos().List()
	if err != nil {
		t.Fatalf("list repos: %v", err)
	}
	if len(repos) != 0 {
		t.Errorf("len(repos) = %d, want 0", len(repos))
	}
}

func TestReposListFiltered(t *testing.T) {
	db := openTestDB(t)

	insertTestRepo(t, db, 1, "alpha", "one")
	insertTestRepo(t, db, 2, "alpha", "two")
	insertTestRepo(t, db, 3, "beta", "three")

	cases := []struct {
		query string
		want  int
	}{
		{"alpha", 2},
		{"three", 1},
		{"nope", 0},
	}

	for _, tc := range cases {
		repos, err := db.Repos().ListFiltered(tc.query)
		if err != nil {
			t.Fatalf("ListFiltered(%q): %v", tc.query, err)
		}
		if len(repos) != tc.want {
			t.Errorf("ListFiltered(%q) returned %d repos, want %d", tc.query, len(repos), tc.want)
		}
	}
}

func TestReposListActive(t *testing.T) {
	db := openTestDB(t)

	activeID := insertTestRepo(t, db, 1, "owner", "active")
	deletedID := insertTestRepo(t, db, 2, "owner", "deleted")

	if err := db.Repos().MarkDeleted(deletedID); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	repos, err := db.Repos().ListActive()
	if err != nil {
		t.Fatalf("list active: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("len(active) = %d, want 1", len(repos))
	}
	if repos[0].ID != activeID {
		t.Errorf("active repo ID = %d, want %d", repos[0].ID, activeID)
	}
}

func TestReposListDeleted(t *testing.T) {
	db := openTestDB(t)

	insertTestRepo(t, db, 1, "owner", "active")
	deletedID := insertTestRepo(t, db, 2, "owner", "deleted")

	if err := db.Repos().MarkDeleted(deletedID); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}

	repos, err := db.Repos().ListDeleted()
	if err != nil {
		t.Fatalf("list deleted: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("len(deleted) = %d, want 1", len(repos))
	}
	if repos[0].ID != deletedID {
		t.Errorf("deleted repo ID = %d, want %d", repos[0].ID, deletedID)
	}
}

func TestReposGetNotFound(t *testing.T) {
	db := openTestDB(t)

	_, err := db.Repos().Get(9999)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get(9999) error = %v, want ErrNotFound", err)
	}
}

func TestReposGetByOwnerName(t *testing.T) {
	db := openTestDB(t)

	insertTestRepo(t, db, 1, "owner", "repo")

	got, err := db.Repos().GetByOwnerName("owner", "repo")
	if err != nil {
		t.Fatalf("get repo by owner/name: %v", err)
	}
	if got.Owner != "owner" || got.Name != "repo" {
		t.Errorf("got %q/%q, want owner/repo", got.Owner, got.Name)
	}

	_, err = db.Repos().GetByOwnerName("owner", "wrong")
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("GetByOwnerName(owner, wrong) error = %v, want ErrNotFound", err)
	}
}

func TestReposSetFormat(t *testing.T) {
	db := openTestDB(t)

	id := insertTestRepo(t, db, 1, "RP2", "gh-vault")
	path := "archived/RP2/gh-vault.bundle"

	if err := db.Repos().SetFormat(id, model.FormatBundle, path); err != nil {
		t.Fatalf("set format: %v", err)
	}

	got, err := db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.Format != model.FormatBundle {
		t.Errorf("Format = %q, want %q", got.Format, model.FormatBundle)
	}
	if got.BackupPath == nil || *got.BackupPath != path {
		t.Errorf("BackupPath = %v, want %q", got.BackupPath, path)
	}
}

func TestReposSetBackupEnabled(t *testing.T) {
	db := openTestDB(t)

	id := insertTestRepo(t, db, 1, "owner", "repo")

	if err := db.Repos().SetBackupEnabled(id, true); err != nil {
		t.Fatalf("set backup enabled true: %v", err)
	}
	got, err := db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if !got.BackupEnabled {
		t.Errorf("BackupEnabled = %v, want true", got.BackupEnabled)
	}

	if err := db.Repos().SetBackupEnabled(id, false); err != nil {
		t.Fatalf("set backup enabled false: %v", err)
	}
	got, err = db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.BackupEnabled {
		t.Errorf("BackupEnabled = %v, want false", got.BackupEnabled)
	}
}

func TestReposSetGitHubMetadata(t *testing.T) {
	db := openTestDB(t)

	id := insertTestRepo(t, db, 1, "owner", "repo")
	now := time.Now().Truncate(time.Second)

	cases := []struct {
		name      string
		sizeKB    int64
		language  *string
		url       *string
		archived  bool
		private   bool
		lastPush  *time.Time
		wantLang  *string
		wantURL   *string
		wantPush  *time.Time
	}{
		{
			name:     "all pointers set",
			sizeKB:   200,
			language: ptr("Python"),
			url:      ptr("https://github.com/new/url"),
			archived: true,
			private:  true,
			lastPush: &now,
			wantLang: ptr("Python"),
			wantURL:  ptr("https://github.com/new/url"),
			wantPush: &now,
		},
		{
			name:     "nil pointers clear values",
			sizeKB:   300,
			language: nil,
			url:      nil,
			archived: false,
			private:  false,
			lastPush: nil,
			wantLang: nil,
			wantURL:  nil,
			wantPush: nil,
		},
		{
			name:     "mixed pointers",
			sizeKB:   400,
			language: ptr("Rust"),
			url:      nil,
			archived: true,
			private:  false,
			lastPush: nil,
			wantLang: ptr("Rust"),
			wantURL:  nil,
			wantPush: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := db.Repos().SetGitHubMetadata(id, tc.sizeKB, tc.language, tc.url, tc.archived, tc.private, tc.lastPush); err != nil {
				t.Fatalf("set github metadata: %v", err)
			}

			got, err := db.Repos().Get(id)
			if err != nil {
				t.Fatalf("get repo: %v", err)
			}
			if got.SizeKB != tc.sizeKB {
				t.Errorf("SizeKB = %d, want %d", got.SizeKB, tc.sizeKB)
			}
			if (got.Language == nil) != (tc.wantLang == nil) || (got.Language != nil && tc.wantLang != nil && *got.Language != *tc.wantLang) {
				t.Errorf("Language = %v, want %v", got.Language, tc.wantLang)
			}
			if (got.GitHubURL == nil) != (tc.wantURL == nil) || (got.GitHubURL != nil && tc.wantURL != nil && *got.GitHubURL != *tc.wantURL) {
				t.Errorf("GitHubURL = %v, want %v", got.GitHubURL, tc.wantURL)
			}
			if got.GitHubArchived != tc.archived {
				t.Errorf("GitHubArchived = %v, want %v", got.GitHubArchived, tc.archived)
			}
			if got.Private != tc.private {
				t.Errorf("Private = %v, want %v", got.Private, tc.private)
			}
			if (got.LastPush == nil) != (tc.wantPush == nil) || (got.LastPush != nil && tc.wantPush != nil && !got.LastPush.Equal(*tc.wantPush)) {
				t.Errorf("LastPush = %v, want %v", got.LastPush, tc.wantPush)
			}
		})
	}
}

func TestReposSetOwnerName(t *testing.T) {
	db := openTestDB(t)

	id := insertTestRepo(t, db, 1, "old-owner", "old-name")

	if err := db.Repos().SetOwnerName(id, "new-owner", "new-name", "https://github.com/new-owner/new-name"); err != nil {
		t.Fatalf("set owner/name: %v", err)
	}

	got, err := db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.Owner != "new-owner" {
		t.Errorf("Owner = %q, want %q", got.Owner, "new-owner")
	}
	if got.Name != "new-name" {
		t.Errorf("Name = %q, want %q", got.Name, "new-name")
	}
	if got.GitHubURL == nil || *got.GitHubURL != "https://github.com/new-owner/new-name" {
		t.Errorf("GitHubURL = %v, want %q", got.GitHubURL, "https://github.com/new-owner/new-name")
	}
}

func TestReposSetBackupPath(t *testing.T) {
	db := openTestDB(t)

	id := insertTestRepo(t, db, 1, "owner", "repo")
	path := "custom/owner/repo.bundle"

	if err := db.Repos().SetBackupPath(id, path); err != nil {
		t.Fatalf("set backup path: %v", err)
	}

	got, err := db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.BackupPath == nil || *got.BackupPath != path {
		t.Errorf("BackupPath = %v, want %q", got.BackupPath, path)
	}
}

func TestReposSetMethodsNotFound(t *testing.T) {
	db := openTestDB(t)
	now := time.Now()

	cases := []struct {
		name string
		call func() error
	}{
		{"SetBackupEnabled", func() error { return db.Repos().SetBackupEnabled(9999, false) }},
		{"SetFormat", func() error { return db.Repos().SetFormat(9999, model.FormatBundle, "path") }},
		{"SetVerified", func() error { return db.Repos().SetVerified(9999, &now) }},
		{"SetLastBackup", func() error { return db.Repos().SetLastBackup(9999, &now) }},
		{"SetGitHubMetadata", func() error { return db.Repos().SetGitHubMetadata(9999, 100, nil, nil, false, false, nil) }},
		{"SetOwnerName", func() error { return db.Repos().SetOwnerName(9999, "owner", "name", "url") }},
		{"SetBackupPath", func() error { return db.Repos().SetBackupPath(9999, "path") }},
		{"MarkDeleted", func() error { return db.Repos().MarkDeleted(9999) }},
		{"UnmarkDeleted", func() error { return db.Repos().UnmarkDeleted(9999) }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if !errors.Is(err, ErrNotFound) {
				t.Errorf("%s(9999) error = %v, want ErrNotFound", tc.name, err)
			}
		})
	}
}

func TestReposMarkDeleted(t *testing.T) {
	db := openTestDB(t)

	id := insertTestRepo(t, db, 1, "owner", "repo")

	if err := db.Repos().MarkDeleted(id); err != nil {
		t.Fatalf("mark deleted: %v", err)
	}
	got, err := db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if !got.GitHubDeleted {
		t.Errorf("GitHubDeleted = %v, want true", got.GitHubDeleted)
	}

	if err := db.Repos().UnmarkDeleted(id); err != nil {
		t.Fatalf("unmark deleted: %v", err)
	}
	got, err = db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.GitHubDeleted {
		t.Errorf("GitHubDeleted = %v, want false", got.GitHubDeleted)
	}
}

func TestReposDelete(t *testing.T) {
	db := openTestDB(t)

	id := insertTestRepo(t, db, 1, "owner", "repo")

	if err := db.Repos().Delete(id); err != nil {
		t.Fatalf("delete repo: %v", err)
	}
	_, err := db.Repos().Get(id)
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("Get after delete error = %v, want ErrNotFound", err)
	}
}

func TestReposSetLastBackup(t *testing.T) {
	db := openTestDB(t)

	id := insertTestRepo(t, db, 1, "owner", "repo")
	now := time.Now().Truncate(time.Second)

	if err := db.Repos().SetLastBackup(id, &now); err != nil {
		t.Fatalf("set last backup: %v", err)
	}

	got, err := db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.LastBackup == nil {
		t.Fatalf("LastBackup is nil")
	}
	if !got.LastBackup.Equal(now) {
		t.Errorf("LastBackup = %v, want %v", got.LastBackup, now)
	}

	if err := db.Repos().SetLastBackup(id, nil); err != nil {
		t.Fatalf("set last backup nil: %v", err)
	}
	got, err = db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.LastBackup != nil {
		t.Errorf("LastBackup = %v, want nil", got.LastBackup)
	}
}

func TestReposSetVerified(t *testing.T) {
	db := openTestDB(t)

	id := insertTestRepo(t, db, 1, "owner", "repo")
	now := time.Now().Truncate(time.Second)

	if err := db.Repos().SetVerified(id, &now); err != nil {
		t.Fatalf("set verified: %v", err)
	}

	got, err := db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.VerifiedAt == nil {
		t.Fatalf("VerifiedAt is nil")
	}
	if !got.VerifiedAt.Equal(now) {
		t.Errorf("VerifiedAt = %v, want %v", got.VerifiedAt, now)
	}

	if err := db.Repos().SetVerified(id, nil); err != nil {
		t.Fatalf("set verified nil: %v", err)
	}
	got, err = db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.VerifiedAt != nil {
		t.Errorf("VerifiedAt = %v, want nil", got.VerifiedAt)
	}
}

func TestReposDuplicateGitHubID(t *testing.T) {
	db := openTestDB(t)

	id, err := db.Repos().Upsert(model.Repo{
		GitHubID: 1,
		Owner:    "RP2",
		Name:     "repo",
		Format:   model.FormatClone,
	})
	if err != nil {
		t.Fatalf("first upsert: %v", err)
	}

	secondID, err := db.Repos().Upsert(model.Repo{
		GitHubID: 1,
		Owner:    "new-owner",
		Name:     "repo",
		Format:   model.FormatClone,
	})
	if err != nil {
		t.Fatalf("second upsert: %v", err)
	}
	if secondID != id {
		t.Errorf("second upsert id = %d, want %d", secondID, id)
	}

	got, err := db.Repos().Get(id)
	if err != nil {
		t.Fatalf("get repo: %v", err)
	}
	if got.Owner != "new-owner" {
		t.Errorf("Owner = %q, want %q", got.Owner, "new-owner")
	}
}
