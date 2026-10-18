package sync

import (
	"context"
	"path/filepath"
	"testing"

	gh "github.com/google/go-github/v69/github"

	"github.com/RP2/gh-vault/internal/model"
	"github.com/RP2/gh-vault/internal/store"
)

// stubClient returns a fixed repository list.
type stubClient struct {
	repos []*gh.Repository
}

func (c stubClient) ListAccessibleRepos(context.Context) ([]*gh.Repository, error) {
	return c.repos, nil
}

func (c stubClient) GetRepo(context.Context, string, string) (*gh.Repository, error) {
	return nil, nil
}

func (c stubClient) RateLimitStatus(context.Context) (*gh.Rate, error) {
	return nil, nil
}

func TestSyncReposEnablesNewReposAsClones(t *testing.T) {
	db, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	client := stubClient{repos: []*gh.Repository{{
		ID:       gh.Ptr(int64(101)),
		Name:     gh.Ptr("fresh"),
		Owner:    &gh.User{Login: gh.Ptr("RP2")},
		Size:     gh.Ptr(42),
		Language: gh.Ptr("Go"),
	}}}
	syncer := NewSyncer(client, db.Repos(), db.Logs(), nil)

	result, err := syncer.SyncRepos(context.Background())
	if err != nil {
		t.Fatalf("sync repos: %v", err)
	}
	if result.Added != 1 {
		t.Fatalf("Added = %d, want 1", result.Added)
	}

	repos, err := db.Repos().List()
	if err != nil {
		t.Fatalf("list repos: %v", err)
	}
	if len(repos) != 1 {
		t.Fatalf("got %d repos, want 1", len(repos))
	}

	repo := repos[0]
	if !repo.BackupEnabled {
		t.Error("BackupEnabled = false, want true so new repos are copied without manual enabling")
	}
	if repo.Format != model.FormatClone {
		t.Errorf("Format = %q, want %q", repo.Format, model.FormatClone)
	}
	wantPath := "active/RP2/fresh.git"
	if repo.BackupPath == nil || *repo.BackupPath != wantPath {
		t.Errorf("BackupPath = %v, want %q", repo.BackupPath, wantPath)
	}
}

func TestPathForFormatClone(t *testing.T) {
	got := PathForFormat(model.FormatClone, "RP2", "gh-vault")
	want := "active/RP2/gh-vault.git"
	if got != want {
		t.Errorf("PathForFormat(clone) = %q, want %q", got, want)
	}
}

func TestPathForFormatBundle(t *testing.T) {
	got := PathForFormat(model.FormatBundle, "RP2", "gh-vault")
	want := "archived/RP2/gh-vault.bundle"
	if got != want {
		t.Errorf("PathForFormat(bundle) = %q, want %q", got, want)
	}
}
