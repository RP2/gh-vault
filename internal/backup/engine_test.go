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

// TestMain isolates git from the developer's own config: a local
// init.defaultBranch, template directory or hooks path would otherwise change
// what these tests see and hide failures that only appear on CI.
func TestMain(m *testing.M) {
	if err := os.Setenv("GIT_CONFIG_GLOBAL", os.DevNull); err != nil {
		panic(err)
	}
	if err := os.Setenv("GIT_CONFIG_SYSTEM", os.DevNull); err != nil {
		panic(err)
	}
	os.Exit(m.Run())
}

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

func TestCopyIsCurrent(t *testing.T) {
	pushed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	other := pushed.Add(time.Hour)

	tests := []struct {
		name string
		repo model.Repo
		want bool
	}{
		{name: "equal timestamps are current", repo: model.Repo{LastPush: &pushed, SourcePushedAt: &pushed}, want: true},
		{name: "newer push is not current", repo: model.Repo{LastPush: &other, SourcePushedAt: &pushed}, want: false},
		{name: "unrecorded copy is not current", repo: model.Repo{LastPush: &pushed}, want: false},
		{name: "repo with no push ever is current", repo: model.Repo{}, want: true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := copyIsCurrent(tc.repo); got != tc.want {
				t.Errorf("copyIsCurrent() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestParseRefs(t *testing.T) {
	out := strings.Join([]string{
		"1111111111111111111111111111111111111111\tHEAD",
		"2222222222222222222222222222222222222222\trefs/heads/main",
		"3333333333333333333333333333333333333333\trefs/remotes/origin/main",
		"4444444444444444444444444444444444444444\trefs/tags/v1.0.0",
		"5555555555555555555555555555555555555555\trefs/tags/v1.0.0^{}",
	}, "\n")

	got := parseRefs(out)
	want := map[string]string{
		"refs/heads/main":  "2222222222222222222222222222222222222222",
		"refs/tags/v1.0.0": "4444444444444444444444444444444444444444",
	}
	if len(got) != len(want) {
		t.Fatalf("parseRefs() = %v, want %v", got, want)
	}
	for ref, sha := range want {
		if got[ref] != sha {
			t.Errorf("parseRefs()[%q] = %q, want %q", ref, got[ref], sha)
		}
	}
}

func TestBackupSkipsUnchangedCopy(t *testing.T) {
	engine, db := newReconcileTestEngine(t)
	engine.tokenProvider = noTokenProvider{}
	repos := db.Repos()

	pushed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	id, err := repos.Upsert(model.Repo{
		GitHubID:      21,
		Owner:         "RP2",
		Name:          "idle",
		Format:        model.FormatClone,
		BackupEnabled: true,
		LastPush:      &pushed,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repos.SetSourcePushedAt(id, &pushed); err != nil {
		t.Fatalf("set source pushed_at: %v", err)
	}
	cloneDir := engine.activePath("RP2", "idle")
	if err := os.MkdirAll(cloneDir, 0o755); err != nil {
		t.Fatalf("mkdir clone: %v", err)
	}
	if err := os.WriteFile(filepath.Join(cloneDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}

	repo, err := repos.Get(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// The engine holds no token: any git work fails instead of reaching the
	// network, so a clean skip proves no git command ran.
	changed, err := engine.Backup(context.Background(), repo, BackupIfChanged)
	if err != nil {
		t.Fatalf("backup: %v", err)
	}
	if changed {
		t.Error("changed = true, want false for a copy already at the pushed_at GitHub reports")
	}

	stored, err := repos.Get(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.LastBackup == nil {
		t.Error("LastBackup = nil, want the check timestamp recorded")
	}
}

// TestVerifyBundleDetectsTruncatedPack covers the gap the header check leaves:
// a bundle cut mid-pack still passes `git bundle verify` but cannot be
// restored, and must fail verification.
func TestVerifyBundleDetectsTruncatedPack(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	engine, db := newReconcileTestEngine(t)
	repo, bundle := newBundleTestRepo(t, engine, db)

	info, err := os.Stat(bundle)
	if err != nil {
		t.Fatalf("stat bundle: %v", err)
	}
	if err := os.Truncate(bundle, info.Size()*6/10); err != nil {
		t.Fatalf("truncate bundle: %v", err)
	}

	if err := engine.Verify(context.Background(), repo); err == nil {
		t.Error("verify of a bundle truncated mid-pack succeeded, want failure")
	}
}

func TestVerifyBundleDeepRecordsSuccess(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	engine, db := newReconcileTestEngine(t)
	repo, _ := newBundleTestRepo(t, engine, db)

	if err := engine.Verify(context.Background(), repo); err != nil {
		t.Fatalf("verify: %v", err)
	}

	stored, err := db.Repos().Get(repo.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.VerifiedAt == nil {
		t.Error("VerifiedAt = nil, want set after a deep verification")
	}

	scratch := filepath.Join(engine.backupDir, ".verify")
	entries, err := os.ReadDir(scratch)
	if err != nil {
		t.Fatalf("read scratch dir: %v", err)
	}
	if len(entries) != 0 {
		t.Errorf("scratch dir left behind: %v", entries)
	}
}

// TestVerifyBundleSkipsWithoutSpace keeps a full volume from being reported as
// a failed backup, and leaves the copy unverified so the dashboard does not
// claim otherwise.
func TestVerifyBundleSkipsWithoutSpace(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	engine, db := newReconcileTestEngine(t)
	repo, _ := newBundleTestRepo(t, engine, db)

	original := freeBytes
	freeBytes = func(string) (uint64, error) { return 1, nil }
	defer func() { freeBytes = original }()

	if err := engine.Verify(context.Background(), repo); err != nil {
		t.Fatalf("verify without space = %v, want a skip", err)
	}

	stored, err := db.Repos().Get(repo.ID)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if stored.VerifiedAt != nil {
		t.Error("VerifiedAt set although verification was skipped")
	}
}

// noTokenProvider supplies no credentials, so any path that needs GitHub fails
// fast instead of reaching the network.
type noTokenProvider struct{}

func (noTokenProvider) GetToken(context.Context) (string, error) { return "", nil }
func (noTokenProvider) SetToken(context.Context, string) error   { return nil }
func (noTokenProvider) Wipe()                                    {}

// TestBackupRebuildsIncompleteCopy covers the self-healing path: a clone left
// without a HEAD by an interrupted run must not be reported as a current copy
// just because the directory exists, even when the recorded pushed_at matches.
func TestBackupRebuildsIncompleteCopy(t *testing.T) {
	engine, db := newReconcileTestEngine(t)
	engine.tokenProvider = noTokenProvider{}
	repos := db.Repos()

	pushed := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	id, err := repos.Upsert(model.Repo{
		GitHubID:      22,
		Owner:         "RP2",
		Name:          "truncated",
		Format:        model.FormatClone,
		BackupEnabled: true,
		LastPush:      &pushed,
	})
	if err != nil {
		t.Fatalf("upsert: %v", err)
	}
	if err := repos.SetSourcePushedAt(id, &pushed); err != nil {
		t.Fatalf("set source pushed_at: %v", err)
	}
	// A clone directory with no HEAD, as an interrupted clone leaves behind.
	if err := os.MkdirAll(engine.activePath("RP2", "truncated"), 0o755); err != nil {
		t.Fatalf("mkdir clone: %v", err)
	}

	repo, err := repos.Get(id)
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// No usable copy: the engine must try to copy, and with no token that
	// attempt fails. A nil error would mean the broken copy was skipped.
	if _, err := engine.Backup(context.Background(), repo, BackupIfChanged); err == nil {
		t.Error("backup of an incomplete clone succeeded, want an attempt to copy")
	}
}

// TestReconcileRemovesRedundantClone covers the upgrade cleanup: a bundle repo
// that an earlier version left with both copies keeps only the verified bundle.
func TestReconcileRemovesRedundantClone(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	engine, db := newReconcileTestEngine(t)
	repo, _ := newBundleTestRepo(t, engine, db)
	now := time.Now()
	if err := db.Repos().SetLastBackup(repo.ID, &now); err != nil {
		t.Fatalf("set last backup: %v", err)
	}

	if err := engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if _, err := os.Stat(engine.activePath("RP2", "bundled")); !os.IsNotExist(err) {
		t.Errorf("redundant clone still present (stat err = %v)", err)
	}
	if _, err := os.Stat(engine.archivedPath("RP2", "bundled")); err != nil {
		t.Errorf("bundle missing after cleanup: %v", err)
	}
}

// TestReconcileKeepsCloneWhenBundleIsBroken is the safety side of that
// cleanup: a bundle that does not verify must not become the only copy.
func TestReconcileKeepsCloneWhenBundleIsBroken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	engine, db := newReconcileTestEngine(t)
	repo, bundle := newBundleTestRepo(t, engine, db)
	now := time.Now()
	if err := db.Repos().SetLastBackup(repo.ID, &now); err != nil {
		t.Fatalf("set last backup: %v", err)
	}
	if err := os.Truncate(bundle, 8); err != nil {
		t.Fatalf("truncate bundle: %v", err)
	}

	if err := engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	if _, err := os.Stat(engine.activePath("RP2", "bundled")); err != nil {
		t.Errorf("clone removed although the bundle does not verify: %v", err)
	}
}

// TestReconcileScrubsStoredToken covers upgrade hygiene: clone configs written
// by an earlier version carried the token, and idle repos never fetch, so the
// scrub cannot wait for a copy to happen.
func TestReconcileScrubsStoredToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	engine, db := newReconcileTestEngine(t)
	repos := db.Repos()
	id, err := repos.Upsert(model.Repo{
		GitHubID:      31,
		Owner:         "RP2",
		Name:          "leaky",
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

	clone := engine.activePath("RP2", "leaky")
	mustGit(t, engine, "init", "--bare", "--quiet", clone)
	mustGit(t, engine, "--git-dir="+clone, "remote", "add", "origin",
		"https://x-access-token:ghp_EXAMPLE@github.com/RP2/leaky.git")

	if err := engine.Reconcile(context.Background()); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	cfg, err := os.ReadFile(filepath.Join(clone, "config"))
	if err != nil {
		t.Fatalf("read clone config: %v", err)
	}
	if strings.Contains(string(cfg), "x-access-token") {
		t.Errorf("clone config still holds a credential:\n%s", cfg)
	}
	want := "url = " + plainURL("RP2", "leaky")
	if !strings.Contains(string(cfg), want) {
		t.Errorf("clone config missing %q:\n%s", want, cfg)
	}
}

func TestCopyUsable(t *testing.T) {
	dir := t.TempDir()

	bare := filepath.Join(dir, "clone.git")
	if err := os.MkdirAll(bare, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	info, err := os.Stat(bare)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if copyUsable(model.FormatClone, bare, info) {
		t.Error("clone without HEAD reported usable")
	}
	if err := os.WriteFile(filepath.Join(bare, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		t.Fatalf("write HEAD: %v", err)
	}
	if !copyUsable(model.FormatClone, bare, info) {
		t.Error("clone with HEAD reported unusable")
	}

	empty := filepath.Join(dir, "empty.bundle")
	if err := os.WriteFile(empty, nil, 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	info, err = os.Stat(empty)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if copyUsable(model.FormatBundle, empty, info) {
		t.Error("empty bundle reported usable")
	}
	if err := os.WriteFile(empty, []byte("# v2 git bundle\n"), 0o644); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	info, err = os.Stat(empty)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if !copyUsable(model.FormatBundle, empty, info) {
		t.Error("non-empty bundle reported unusable")
	}
}

// TestFetchOriginCopiesLatestRefs exercises the real fetch path against a local
// remote: the authenticated URL is passed per invocation, so a clone whose
// stored origin is the credential-free URL still updates.
func TestFetchOriginCopiesLatestRefs(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	engine, _ := newReconcileTestEngine(t)
	ctx := context.Background()

	work := filepath.Join(t.TempDir(), "work")
	mustGit(t, engine, "init", "--quiet", work)
	mustGit(t, engine, "-C", work,
		"-c", "user.email=test@example.com", "-c", "user.name=test",
		"commit", "--quiet", "--allow-empty", "-m", "one")
	mustGit(t, engine, "-C", work, "tag", "v1")

	local := filepath.Join(t.TempDir(), "local.git")
	mustGit(t, engine, "clone", "--quiet", "--bare", work, local)
	mustGit(t, engine, "--git-dir="+local, "remote", "set-url", "origin", plainURL("RP2", "link-index"))

	mustGit(t, engine, "-C", work,
		"-c", "user.email=test@example.com", "-c", "user.name=test",
		"commit", "--quiet", "--allow-empty", "-m", "two")

	if err := engine.fetchOrigin(ctx, local, work); err != nil {
		t.Fatalf("fetch origin: %v", err)
	}

	// Compare HEAD commits rather than a branch name: `git init` picks the
	// default branch from the environment's config, which differs between a
	// developer machine and a CI runner.
	fetched, err := engine.gitOutput(ctx, []string{"--git-dir=" + local, "rev-parse", "HEAD"}, "")
	if err != nil {
		t.Fatalf("read fetched HEAD: %v", err)
	}
	want, err := engine.gitOutput(ctx, []string{"-C", work, "rev-parse", "HEAD"}, "")
	if err != nil {
		t.Fatalf("read work HEAD: %v", err)
	}
	if fetched != want {
		t.Errorf("fetched HEAD = %s, want %s", fetched, want)
	}

	origin, err := engine.gitOutput(ctx, []string{"--git-dir=" + local, "remote", "get-url", "origin"}, "")
	if err != nil {
		t.Fatalf("read origin: %v", err)
	}
	if strings.Contains(origin, "x-access-token") {
		t.Errorf("origin = %q, want no credentials after a fetch", origin)
	}
}

// TestEnsureOriginScrubsToken guards the credential-free clone config: a token
// stored by an earlier version must be replaced by the plain URL.
func TestEnsureOriginScrubsToken(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	engine, _ := newReconcileTestEngine(t)
	dir := filepath.Join(t.TempDir(), "clone.git")
	mustGit(t, engine, "init", "--bare", "--quiet", dir)
	mustGit(t, engine, "--git-dir="+dir, "remote", "add", "origin",
		"https://x-access-token:ghp_EXAMPLE@github.com/RP2/link-index.git")

	if err := engine.ensureOrigin(context.Background(), dir, "RP2", "link-index"); err != nil {
		t.Fatalf("ensure origin: %v", err)
	}

	out, err := engine.gitOutput(context.Background(), []string{"--git-dir=" + dir, "remote", "get-url", "origin"}, "")
	if err != nil {
		t.Fatalf("read origin: %v", err)
	}
	if strings.Contains(out, "ghp_EXAMPLE") || strings.Contains(out, "x-access-token") {
		t.Errorf("origin = %q, want no credentials stored", out)
	}
	want := "https://github.com/RP2/link-index.git"
	if out != want {
		t.Errorf("origin = %q, want %q", out, want)
	}
}

// TestEnsureOriginAddsMissingRemote covers clones with no origin at all, which
// fetch would otherwise fail on.
func TestEnsureOriginAddsMissingRemote(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not installed")
	}

	engine, _ := newReconcileTestEngine(t)
	dir := filepath.Join(t.TempDir(), "clone.git")
	mustGit(t, engine, "init", "--bare", "--quiet", dir)

	if err := engine.ensureOrigin(context.Background(), dir, "RP2", "link-index"); err != nil {
		t.Fatalf("ensure origin: %v", err)
	}

	out, err := engine.gitOutput(context.Background(), []string{"--git-dir=" + dir, "remote", "get-url", "origin"}, "")
	if err != nil {
		t.Fatalf("read origin: %v", err)
	}
	if out != "https://github.com/RP2/link-index.git" {
		t.Errorf("origin = %q, want the plain URL", out)
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
