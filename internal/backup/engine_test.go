package backup

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// mustGit runs a git command through the engine runner and fails the test on
// error.
func mustGit(t *testing.T, engine *BackupEngine, args ...string) {
	t.Helper()
	if err := engine.runGit(context.Background(), args, ""); err != nil {
		t.Fatalf("git %s: %v", strings.Join(args, " "), err)
	}
}

// newBundleTestRepo creates the artifacts a real bundle backup leaves behind: a
// bare clone under active/ and a bundle created from it with --all, matching
// createBundle.
func newBundleTestRepo(t *testing.T, engine *BackupEngine, db *store.DB) (model.Repo, string) {
	t.Helper()

	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, engine, "init", "--quiet", work)
	mustGit(t, engine, "-C", work,
		"-c", "user.email=test@example.com", "-c", "user.name=test",
		"commit", "--quiet", "--allow-empty", "-m", "one")

	clone := engine.activePath("RP2", "bundled")
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatalf("mkdir active: %v", err)
	}
	mustGit(t, engine, "clone", "--quiet", "--bare", work, clone)

	bundle := engine.archivedPath("RP2", "bundled")
	if err := os.MkdirAll(filepath.Dir(bundle), 0o755); err != nil {
		t.Fatalf("mkdir archived: %v", err)
	}
	mustGit(t, engine, "--git-dir="+clone, "bundle", "create", bundle, "--all")

	repos := db.Repos()
	id, err := repos.Upsert(model.Repo{
		GitHubID:      4,
		Owner:         "RP2",
		Name:          "bundled",
		Format:        model.FormatBundle,
		BackupEnabled: true,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	repo, err := repos.Get(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	return repo, bundle
}

func TestVerifyBundleWithoutRepositoryContext(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	engine, db := newReconcileTestEngine(t)

	// The container runs from a working directory that is not inside a git
	// repository. Verification must not depend on the process working
	// directory, so move the test out of the gh-vault checkout.
	t.Chdir(t.TempDir())

	repo, _ := newBundleTestRepo(t, engine, db)

	if err := engine.Verify(context.Background(), repo); err != nil {
		t.Fatalf("verify bundle: %v", err)
	}

	stored, err := db.Repos().Get(repo.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.VerifiedAt == nil {
		t.Error("VerifiedAt = nil, want set after successful verification")
	}
}

func TestVerifyBundleRejectsCorruptBundle(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	engine, db := newReconcileTestEngine(t)
	t.Chdir(t.TempDir())

	repo, bundle := newBundleTestRepo(t, engine, db)
	if err := os.Truncate(bundle, 8); err != nil {
		t.Fatalf("truncate bundle: %v", err)
	}

	if err := engine.Verify(context.Background(), repo); err == nil {
		t.Error("verify corrupt bundle: got nil error, want failure")
	}
}
