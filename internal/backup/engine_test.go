package backup

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/RP2/gh-vault/internal/model"
	"github.com/RP2/gh-vault/internal/store"
)

// newReconcileTestEngine builds an engine over a real SQLite store and an
// empty backup directory. The token provider is nil: Reconcile never touches
// credentials.
func newReconcileTestEngine(t *testing.T) (*BackupEngine, *store.DB) {
	t.Helper()

	db, err := store.New(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	root := t.TempDir()
	for _, dir := range []string{
		filepath.Join(root, "active", "RP2"),
		filepath.Join(root, "archived", "RP2"),
	} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("mkdir %s: %v", dir, err)
		}
	}
	return NewEngine(root, nil, db.Repos()), db
}

func TestReconcileResetsStateForMissingFiles(t *testing.T) {
	engine, db := newReconcileTestEngine(t)
	repos := db.Repos()

	id, err := repos.Upsert(model.Repo{
		GitHubID:      1,
		Owner:         "RP2",
		Name:          "gone",
		Format:        model.FormatClone,
		BackupEnabled: true,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := time.Now()
	if err := repos.SetLastBackup(id, &now); err != nil {
		t.Fatalf("set last backup: %v", err)
	}
	if err := repos.SetVerified(id, &now); err != nil {
		t.Fatalf("set verified: %v", err)
	}

	if err := engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	repo, err := repos.Get(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if repo.LastBackup != nil {
		t.Errorf("LastBackup = %v, want nil after reconcile with missing files", repo.LastBackup)
	}
	if repo.VerifiedAt != nil {
		t.Errorf("VerifiedAt = %v, want nil after reconcile with missing files", repo.VerifiedAt)
	}
}

func TestReconcileKeepsStateWhenFileExists(t *testing.T) {
	engine, db := newReconcileTestEngine(t)
	repos := db.Repos()

	id, err := repos.Upsert(model.Repo{
		GitHubID:      2,
		Owner:         "RP2",
		Name:          "present",
		Format:        model.FormatClone,
		BackupEnabled: true,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := time.Now()
	if err := repos.SetLastBackup(id, &now); err != nil {
		t.Fatalf("set last backup: %v", err)
	}

	cloneDir := filepath.Join(engine.backupDir, "active", "RP2", "present.git")
	if err := os.MkdirAll(cloneDir, 0o755); err != nil {
		t.Fatalf("mkdir clone dir: %v", err)
	}

	if err := engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	repo, err := repos.Get(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if repo.LastBackup == nil {
		t.Error("LastBackup = nil, want preserved when the file exists on disk")
	}
}

func TestReconcileResetsMissingBundleState(t *testing.T) {
	engine, db := newReconcileTestEngine(t)
	repos := db.Repos()

	id, err := repos.Upsert(model.Repo{
		GitHubID:      3,
		Owner:         "RP2",
		Name:          "bundled",
		Format:        model.FormatBundle,
		BackupEnabled: true,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	now := time.Now()
	if err := repos.SetLastBackup(id, &now); err != nil {
		t.Fatalf("set last backup: %v", err)
	}

	if err := engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	repo, err := repos.Get(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if repo.LastBackup != nil {
		t.Errorf("LastBackup = %v, want nil when the bundle file is missing", repo.LastBackup)
	}
}
