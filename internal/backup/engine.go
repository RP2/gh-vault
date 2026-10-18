// Package backup implements the repository backup engine for gh-vault.
// It performs bare clones, fetches, bundle creation, and format switching
// using the git CLI. All git authentication is performed with an ephemeral
// token injected into the remote URL; the token is never logged or exposed.
package backup

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/RP2/gh-vault/internal/github"
	"github.com/RP2/gh-vault/internal/model"
	"github.com/RP2/gh-vault/internal/store"
	syncpkg "github.com/RP2/gh-vault/internal/sync"
)

// BackupMode selects how hard a backup works to notice that a repository
// changed. GitHub's pushed_at is the cheap signal; comparing refs is the
// reliable one.
type BackupMode int

const (
	// BackupIfChanged copies only when GitHub's pushed_at differs from the
	// pushed_at the local copy represents. An idle repository costs no git
	// work at all.
	BackupIfChanged BackupMode = iota
	// BackupVerifyRefs additionally asks the remote for its refs and copies
	// when they differ from the local copy, catching pushes that leave
	// pushed_at unchanged, such as tag-only pushes.
	BackupVerifyRefs
)

// Engine performs backup operations on a repository.
type Engine interface {
	Backup(ctx context.Context, repo model.Repo, mode BackupMode) (bool, error)
	SwitchToBundle(ctx context.Context, repo model.Repo) error
	SwitchToClone(ctx context.Context, repo model.Repo) error
	Verify(ctx context.Context, repo model.Repo) error
	RemoveLocal(owner, name string) error
	Reconcile(ctx context.Context) error
}

// BackupEngine implements Engine using the git CLI.
type BackupEngine struct {
	backupDir     string
	tokenProvider github.TokenProvider
	repos         store.RepoStore
}

// NewEngine creates a new BackupEngine.
func NewEngine(backupDir string, tokenProvider github.TokenProvider, repos store.RepoStore) *BackupEngine {
	return &BackupEngine{
		backupDir:     backupDir,
		tokenProvider: tokenProvider,
		repos:         repos,
	}
}

var _ Engine = (*BackupEngine)(nil)

// authURLPattern matches the token-bearing portion of an authenticated GitHub
// remote URL so it can be redacted from command output and error messages.
var authURLPattern = regexp.MustCompile(`x-access-token:[^@]+@`)

// redactAuthURL removes the GitHub token from a string.
func redactAuthURL(s string) string {
	return authURLPattern.ReplaceAllString(s, "x-access-token:REDACTED@")
}

// normalizeRemoteURL strips the userinfo section from a GitHub remote URL so
// URLs with different tokens can be compared.
func normalizeRemoteURL(s string) string {
	return regexp.MustCompile(`://[^@]+@`).ReplaceAllString(s, "://")
}

// plainURL returns the credential-free remote URL for a repository. This is the
// URL stored in a clone's config: git writes remote URLs verbatim, so storing
// the token-bearing URL would leave a live token in plaintext in every clone.
func plainURL(owner, name string) string {
	return fmt.Sprintf("https://github.com/%s/%s.git", owner, name)
}

// authURL returns an authenticated GitHub remote URL for the repository. It is
// passed to git per invocation and never stored, so the token stays out of
// clone configs on the backup volume.
func (e *BackupEngine) authURL(ctx context.Context, owner, name string) (string, error) {
	token, err := e.tokenProvider.GetToken(ctx)
	if err != nil {
		return "", err
	}
	if token == "" {
		return "", errors.New("backup: token not configured")
	}
	return fmt.Sprintf("https://x-access-token:%s@github.com/%s/%s.git", token, owner, name), nil
}

// ensureOrigin points a clone's origin remote at the credential-free URL,
// scrubbing any token an earlier version stored there. The authenticated URL is
// supplied per git invocation instead; see fetchOrigin.
func (e *BackupEngine) ensureOrigin(ctx context.Context, gitDir, owner, name string) error {
	want := plainURL(owner, name)

	out, err := e.gitOutput(ctx, []string{"--git-dir=" + gitDir, "remote"}, "")
	if err != nil {
		return fmt.Errorf("list remotes: %w", err)
	}
	hasOrigin := false
	for _, line := range strings.Split(out, "\n") {
		if strings.TrimSpace(line) == "origin" {
			hasOrigin = true
			break
		}
	}
	if !hasOrigin {
		if err := e.runGit(ctx, []string{"--git-dir=" + gitDir, "remote", "add", "origin", want}, ""); err != nil {
			return fmt.Errorf("add origin: %w", err)
		}
		return nil
	}

	current, err := e.gitOutput(ctx, []string{"--git-dir=" + gitDir, "remote", "get-url", "origin"}, "")
	if err != nil {
		return fmt.Errorf("read origin: %w", err)
	}
	if strings.TrimSpace(current) == want {
		return nil
	}
	if err := e.runGit(ctx, []string{"--git-dir=" + gitDir, "remote", "set-url", "origin", want}, ""); err != nil {
		return fmt.Errorf("set origin: %w", err)
	}
	return nil
}

// fetchOrigin fetches with the authenticated URL for this invocation only, so
// the token reaches git without being written to disk. The remote name is left
// out on purpose: `git -c remote.origin.url=... fetch origin` does not override
// the stored URL, so the URL and refspec are passed explicitly. The refspec
// matches what a bare clone's origin uses, and --prune drops deleted branches;
// tags are auto-followed as before.
func (e *BackupEngine) fetchOrigin(ctx context.Context, gitDir, url string) error {
	return e.runGit(ctx, []string{
		"--git-dir=" + gitDir, "fetch", url,
		"--prune", "+refs/heads/*:refs/heads/*",
	}, "")
}

// ensureClone makes path a bare clone of the repository that is current with
// GitHub, cloning it when absent and fetching it otherwise.
func (e *BackupEngine) ensureClone(ctx context.Context, repo model.Repo, path string) error {
	url, err := e.authURL(ctx, repo.Owner, repo.Name)
	if err != nil {
		return err
	}

	info, err := os.Stat(path)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("target %s exists but is not a directory", path)
		}
		// Bare repos store HEAD directly in the directory; a missing HEAD means
		// the directory is stale and should be recloned.
		if _, err := os.Stat(filepath.Join(path, "HEAD")); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("stat HEAD: %w", err)
			}
			slog.Warn("backup: clone missing HEAD, removing stale directory", "path", path)
			if err := os.RemoveAll(path); err != nil {
				return fmt.Errorf("remove stale clone: %w", err)
			}
			return e.cloneFresh(ctx, repo, path, url)
		}
		if err := e.ensureOrigin(ctx, path, repo.Owner, repo.Name); err != nil {
			return err
		}
		if err := e.fetchOrigin(ctx, path, url); err != nil {
			return fmt.Errorf("fetch: %w", err)
		}
		return nil
	}
	if !os.IsNotExist(err) {
		return fmt.Errorf("stat target: %w", err)
	}
	return e.cloneFresh(ctx, repo, path, url)
}

// cloneFresh clones the repository into path and immediately replaces the
// token-bearing URL git stores with the credential-free one, so a fresh clone
// does not keep a live credential on the backup volume.
func (e *BackupEngine) cloneFresh(ctx context.Context, repo model.Repo, path, url string) error {
	if err := e.runGit(ctx, []string{"clone", "--bare", url, path}, ""); err != nil {
		return fmt.Errorf("clone: %w", err)
	}
	return e.ensureOrigin(ctx, path, repo.Owner, repo.Name)
}

// activePath returns the absolute path for a bare clone backup.
func (e *BackupEngine) activePath(owner, name string) string {
	return filepath.Join(e.backupDir, "active", owner, name+".git")
}

// archivedPath returns the absolute path for a bundle backup.
func (e *BackupEngine) archivedPath(owner, name string) string {
	return filepath.Join(e.backupDir, "archived", owner, name+".bundle")
}

// runGit executes a git command with context and returns an error that never
// includes the GitHub token.
func (e *BackupEngine) runGit(ctx context.Context, args []string, dir string) error {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		outStr := redactAuthURL(string(out))
		redacted := redactAuthURL(strings.Join(args, " "))
		if len(outStr) > 0 {
			return fmt.Errorf("git %s: %w: %s", redacted, err, outStr)
		}
		return fmt.Errorf("git %s: %w", redacted, err)
	}
	return nil
}

// gitOutput executes a git command and returns its redacted stdout.
func (e *BackupEngine) gitOutput(ctx context.Context, args []string, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", args...)
	if dir != "" {
		cmd.Dir = dir
	}
	out, err := cmd.CombinedOutput()
	outStr := strings.TrimSpace(redactAuthURL(string(out)))
	if err != nil {
		redacted := redactAuthURL(strings.Join(args, " "))
		return "", fmt.Errorf("git %s: %w: %s", redacted, err, outStr)
	}
	return outStr, nil
}

// Backup brings a repository's local copy up to date and reports whether data
// was transferred. mode selects how hard it works to notice a change; a copy
// that is already current costs no git work beyond an optional ref comparison.
func (e *BackupEngine) Backup(ctx context.Context, repo model.Repo, mode BackupMode) (bool, error) {
	var path string
	switch repo.Format {
	case model.FormatClone:
		path = e.activePath(repo.Owner, repo.Name)
	case model.FormatBundle:
		path = e.archivedPath(repo.Owner, repo.Name)
	default:
		return false, fmt.Errorf("backup: %s/%s: unknown format %q", repo.Owner, repo.Name, repo.Format)
	}

	info, statErr := os.Stat(path)
	switch {
	case statErr != nil && !os.IsNotExist(statErr):
		return false, fmt.Errorf("backup: %s/%s: stat %s: %w", repo.Owner, repo.Name, path, statErr)
	case statErr == nil && copyUsable(repo.Format, path, info) && !e.sourceChanged(ctx, repo, path, mode):
		return false, e.recordChecked(repo)
	}

	if repo.Format == model.FormatBundle {
		if err := e.refreshBundle(ctx, repo); err != nil {
			return false, err
		}
	} else {
		if err := e.cloneRepo(ctx, repo); err != nil {
			return false, err
		}
	}
	return true, e.recordCopied(repo)
}

// copyUsable reports whether the file at path has the shape of a complete copy:
// a bare clone directory with a HEAD, or a non-empty bundle file. A leftover
// from a crash must not be trusted just because a path exists, or the skip path
// would report a broken backup as current instead of rebuilding it.
func copyUsable(format model.RepoFormat, path string, info os.FileInfo) bool {
	if format == model.FormatBundle {
		return !info.IsDir() && info.Size() > 0
	}
	if !info.IsDir() {
		return false
	}
	_, err := os.Stat(filepath.Join(path, "HEAD"))
	return err == nil
}

// sourceChanged reports whether the local copy needs refreshing.
func (e *BackupEngine) sourceChanged(ctx context.Context, repo model.Repo, path string, mode BackupMode) bool {
	if mode == BackupIfChanged {
		// Cheap signal: the pushed_at this copy represents against the one
		// sync last saw.
		return !copyIsCurrent(repo)
	}

	// Reliable signal: ask the remote for its refs. A failed comparison means
	// "assume changed": copying is cheaper than silently skipping a backup.
	differ, err := e.refsDiffer(ctx, repo, path)
	if err != nil {
		slog.Warn("backup: ref comparison failed, copying anyway", "owner", repo.Owner, "name", repo.Name, "error", err)
		return true
	}
	return differ
}

// copyIsCurrent reports whether the local copy represents the pushed_at that
// sync last saw. Absent timestamps on both sides mean GitHub reports no push
// ever, and a copy of an empty repository is still current.
func copyIsCurrent(repo model.Repo) bool {
	if repo.SourcePushedAt == nil || repo.LastPush == nil {
		return repo.SourcePushedAt == nil && repo.LastPush == nil
	}
	return repo.SourcePushedAt.Equal(*repo.LastPush)
}

// recordChecked stores that the local copy was compared against the remote and
// needed no work, keeping last_backup meaningful for the dashboard.
func (e *BackupEngine) recordChecked(repo model.Repo) error {
	now := time.Now().UTC()
	if err := e.repos.SetLastBackup(repo.ID, &now); err != nil {
		return fmt.Errorf("backup: %s/%s: set last backup: %w", repo.Owner, repo.Name, err)
	}
	if !copyIsCurrent(repo) {
		if err := e.repos.SetSourcePushedAt(repo.ID, repo.LastPush); err != nil {
			return fmt.Errorf("backup: %s/%s: set source pushed_at: %w", repo.Owner, repo.Name, err)
		}
	}
	slog.Info("backup: copy already current, nothing to do", "owner", repo.Owner, "name", repo.Name)
	return nil
}

// recordCopied stores the source pushed_at the new copy represents.
func (e *BackupEngine) recordCopied(repo model.Repo) error {
	now := time.Now().UTC()
	if err := e.repos.SetLastBackup(repo.ID, &now); err != nil {
		return fmt.Errorf("backup: %s/%s: set last backup: %w", repo.Owner, repo.Name, err)
	}
	if err := e.repos.SetSourcePushedAt(repo.ID, repo.LastPush); err != nil {
		return fmt.Errorf("backup: %s/%s: set source pushed_at: %w", repo.Owner, repo.Name, err)
	}
	return nil
}

// refsDiffer reports whether the remote advertises refs the local copy does not
// represent. Local-only refs are ignored: fetch prunes deleted branches, and a
// stale tag is harmless history, so flagging either would copy the repository
// on every run.
func (e *BackupEngine) refsDiffer(ctx context.Context, repo model.Repo, path string) (bool, error) {
	url, err := e.authURL(ctx, repo.Owner, repo.Name)
	if err != nil {
		return false, err
	}

	remoteOut, err := e.gitOutput(ctx, []string{"ls-remote", "--heads", "--tags", url}, "")
	if err != nil {
		return false, fmt.Errorf("ls-remote: %w", err)
	}

	var localOut string
	if repo.Format == model.FormatBundle {
		localOut, err = e.gitOutput(ctx, []string{"bundle", "list-heads", path}, "")
	} else {
		localOut, err = e.gitOutput(ctx, []string{
			"--git-dir=" + path, "for-each-ref",
			"--format=%(objectname) %(refname)", "refs/heads", "refs/tags",
		}, "")
	}
	if err != nil {
		return false, fmt.Errorf("read local refs: %w", err)
	}

	local := parseRefs(localOut)
	for ref, sha := range parseRefs(remoteOut) {
		if local[ref] != sha {
			return true, nil
		}
	}
	return false, nil
}

// parseRefs maps branch and tag names to object names, ignoring HEAD, peeled
// tag entries (refs/tags/v1^{}) and remote-tracking refs so that ls-remote,
// for-each-ref and bundle output can be compared directly.
func parseRefs(out string) map[string]string {
	refs := make(map[string]string)
	for _, line := range strings.Split(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		sha, ref := fields[0], fields[1]
		if strings.HasSuffix(ref, "^{}") {
			continue
		}
		if !strings.HasPrefix(ref, "refs/heads/") && !strings.HasPrefix(ref, "refs/tags/") {
			continue
		}
		refs[ref] = sha
	}
	return refs
}

// cloneRepo clones or fetches the bare clone and clears the verified flag,
// because the copy may have changed.
func (e *BackupEngine) cloneRepo(ctx context.Context, repo model.Repo) error {
	target := e.activePath(repo.Owner, repo.Name)
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("backup: clone %s/%s: create dir: %w", repo.Owner, repo.Name, err)
	}
	if err := e.ensureClone(ctx, repo, target); err != nil {
		slog.Error("backup: clone or fetch failed", "owner", repo.Owner, "name", repo.Name, "error", err)
		return fmt.Errorf("backup: clone %s/%s: %w", repo.Owner, repo.Name, err)
	}
	slog.Info("backup: clone or fetch succeeded", "owner", repo.Owner, "name", repo.Name)
	if err := e.repos.SetVerified(repo.ID, nil); err != nil {
		slog.Error("backup: clear verified_at", "repo", repo.Name, "error", err)
	}
	return nil
}

// createBundle writes a bundle of every ref in source to target.
func (e *BackupEngine) createBundle(ctx context.Context, source, target string) error {
	info, err := os.Stat(source)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("source not found")
		}
		return fmt.Errorf("stat source: %w", err)
	}
	if !info.IsDir() {
		return fmt.Errorf("source is not a directory")
	}

	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("create dir: %w", err)
	}

	if err := e.runGit(ctx, []string{"--git-dir=" + source, "bundle", "create", target, "--all"}, ""); err != nil {
		return fmt.Errorf("git bundle create: %w", err)
	}
	return nil
}

// refreshBundle rebuilds a bundle from GitHub and promotes it atomically. A
// bare clone is scratch space only: a bundle-format repository is cold storage
// and keeps a single, credential-free copy on disk.
func (e *BackupEngine) refreshBundle(ctx context.Context, repo model.Repo) error {
	owner, name := repo.Owner, repo.Name
	final := e.archivedPath(owner, name)
	temp := filepath.Join(filepath.Dir(final), fmt.Sprintf(".%s.%d.tmp", name, time.Now().UnixNano()))

	if err := os.MkdirAll(filepath.Dir(final), 0755); err != nil {
		return fmt.Errorf("backup: bundle %s/%s: create dir: %w", owner, name, err)
	}

	// Reuse a clone left over from an earlier format switch instead of
	// downloading the repository again; otherwise work in a scratch directory.
	scratch := e.activePath(owner, name)
	reused := false
	if info, err := os.Stat(scratch); err == nil && info.IsDir() {
		reused = true
	} else {
		scratch = filepath.Join(filepath.Dir(scratch), fmt.Sprintf(".%s.%d.tmp", name, time.Now().UnixNano()))
	}
	if !reused {
		if err := os.MkdirAll(filepath.Dir(scratch), 0755); err != nil {
			return fmt.Errorf("backup: bundle %s/%s: create scratch dir: %w", owner, name, err)
		}
	}

	promoted := false
	defer func() {
		if err := os.Remove(temp); err != nil && !os.IsNotExist(err) {
			slog.Warn("backup: remove temporary bundle", "path", temp, "error", err)
		}
		// A reused clone is the repository's only copy until the bundle is in
		// place, so it is removed only after a successful promotion.
		if !reused || promoted {
			if err := os.RemoveAll(scratch); err != nil {
				slog.Warn("backup: remove scratch clone", "path", scratch, "error", err)
			}
		}
	}()

	if err := e.ensureClone(ctx, repo, scratch); err != nil {
		return fmt.Errorf("backup: bundle %s/%s: %w", owner, name, err)
	}

	// Step 1: check the objects we are about to archive, while they are still
	// in the page cache from the clone or fetch.
	if err := e.runGit(ctx, []string{"--git-dir=" + scratch, "fsck", "--no-dangling"}, ""); err != nil {
		return fmt.Errorf("backup: bundle %s/%s: fsck: %w", owner, name, err)
	}

	// Step 2: build the bundle next to its destination.
	if err := e.createBundle(ctx, scratch, temp); err != nil {
		return fmt.Errorf("backup: bundle %s/%s: %w", owner, name, err)
	}

	// Step 3: prove the bundle stands alone.
	if err := e.verifyBundle(ctx, temp); err != nil {
		return fmt.Errorf("backup: bundle %s/%s: %w", owner, name, err)
	}

	// Step 4: promote it.
	if err := os.Rename(temp, final); err != nil {
		return fmt.Errorf("backup: bundle %s/%s: promote: %w", owner, name, err)
	}
	promoted = true

	slog.Info("backup: bundle refreshed", "owner", owner, "name", name)
	now := time.Now().UTC()
	if err := e.repos.SetVerified(repo.ID, &now); err != nil {
		slog.Error("backup: set verified_at", "repo", name, "error", err)
	}
	return nil
}

// verifyBundle checks that a bundle file is readable and self-contained.

// SwitchToBundle atomically replaces a bare clone with a verified bundle.
func (e *BackupEngine) SwitchToBundle(ctx context.Context, repo model.Repo) error {
	owner, name := repo.Owner, repo.Name
	source := e.activePath(owner, name)
	final := e.archivedPath(owner, name)
	temp := filepath.Join(filepath.Dir(final), fmt.Sprintf(".bundle.%d.tmp", time.Now().UnixNano()))

	cleanup := func() {
		_ = os.RemoveAll(temp)
	}
	defer cleanup()

	// Step 1: create bundle to temp path.
	if err := e.createBundle(ctx, source, temp); err != nil {
		return fmt.Errorf("backup: switch to bundle %s/%s step 1: %w", owner, name, err)
	}

	// Step 2: verify the bundle stands alone before the clone is removed.
	if err := e.verifyBundle(ctx, temp); err != nil {
		return fmt.Errorf("backup: switch to bundle %s/%s step 2: %w", owner, name, err)
	}

	// Step 3: rename temp to final.
	if err := os.Rename(temp, final); err != nil {
		return fmt.Errorf("backup: switch to bundle %s/%s step 3: %w", owner, name, err)
	}

	// Step 4: update the stored format and clear verification.
	newPath := syncpkg.PathForFormat(model.FormatBundle, owner, name)
	if err := e.repos.SetFormat(repo.ID, model.FormatBundle, newPath); err != nil {
		return fmt.Errorf("backup: switch to bundle %s/%s step 4: %w", owner, name, err)
	}
	if err := e.repos.SetVerified(repo.ID, nil); err != nil {
		return fmt.Errorf("backup: switch to bundle %s/%s step 4: %w", owner, name, err)
	}

	// Step 5: remove the bare clone directory.
	if err := os.RemoveAll(source); err != nil {
		return fmt.Errorf("backup: switch to bundle %s/%s step 5: %w", owner, name, err)
	}
	now := time.Now().UTC()
	if err := e.repos.SetLastBackup(repo.ID, &now); err != nil {
		slog.Error("switch: set last_backup", "repo", repo.Name, "error", err)
	}
	return nil
}

// verifyBareRepo checks that path is a bare clone whose origin remote points to
// the expected repository.
func (e *BackupEngine) verifyBareRepo(ctx context.Context, path, authURL, owner, name string) error {
	out, err := e.gitOutput(ctx, []string{"--git-dir=" + path, "rev-parse", "--is-bare-repository"}, "")
	if err != nil {
		return fmt.Errorf("check bare repo: %w", err)
	}
	if out != "true" {
		return fmt.Errorf("not a bare repository")
	}

	out, err = e.gitOutput(ctx, []string{"--git-dir=" + path, "remote", "get-url", "origin"}, "")
	if err != nil {
		return fmt.Errorf("get remote url: %w", err)
	}
	if normalizeRemoteURL(out) != normalizeRemoteURL(authURL) {
		return fmt.Errorf("remote url mismatch")
	}
	return nil
}

// SwitchToClone restores a bundle to a bare clone and updates the origin remote.
func (e *BackupEngine) SwitchToClone(ctx context.Context, repo model.Repo) error {
	owner, name := repo.Owner, repo.Name
	authURL, err := e.authURL(ctx, owner, name)
	if err != nil {
		return fmt.Errorf("backup: switch to clone %s/%s: %w", owner, name, err)
	}

	bundlePath := e.archivedPath(owner, name)
	final := e.activePath(owner, name)
	temp := fmt.Sprintf("%s.%d.tmp", final, time.Now().UnixNano())

	cleanup := func() {
		_ = os.RemoveAll(temp)
	}
	defer cleanup()

	if err := os.RemoveAll(temp); err != nil {
		return fmt.Errorf("backup: switch to clone %s/%s: %w", owner, name, err)
	}

	// Step 1: if a clone already exists, verify it and remove it.
	if info, err := os.Stat(final); err == nil {
		if !info.IsDir() {
			return fmt.Errorf("backup: switch to clone %s/%s step 1: existing path is not a directory", owner, name)
		}
		if err := e.verifyBareRepo(ctx, final, authURL, owner, name); err != nil {
			return fmt.Errorf("backup: switch to clone %s/%s step 1: %w", owner, name, err)
		}
		if err := os.RemoveAll(final); err != nil {
			return fmt.Errorf("backup: switch to clone %s/%s step 1: %w", owner, name, err)
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("backup: switch to clone %s/%s step 1: %w", owner, name, err)
	}

	// Step 2: clone the bundle into a temporary directory.
	if err := e.runGit(ctx, []string{"clone", "--bare", bundlePath, temp}, ""); err != nil {
		return fmt.Errorf("backup: switch to clone %s/%s step 2: %w", owner, name, err)
	}

	// Step 3: point origin at the credential-free remote and fetch the latest
	// refs, supplying the authenticated URL for this run only.
	if err := e.ensureOrigin(ctx, temp, owner, name); err != nil {
		return fmt.Errorf("backup: switch to clone %s/%s step 3: %w", owner, name, err)
	}

	// Step 4: fetch the latest refs from GitHub.
	if err := e.fetchOrigin(ctx, temp, authURL); err != nil {
		return fmt.Errorf("backup: switch to clone %s/%s step 4: %w", owner, name, err)
	}

	// Step 5: verify repository integrity.
	if err := e.runGit(ctx, []string{"--git-dir=" + temp, "fsck", "--no-dangling"}, ""); err != nil {
		return fmt.Errorf("backup: switch to clone %s/%s step 5: %w", owner, name, err)
	}

	// Step 6: atomically promote the temporary clone.
	if err := os.Rename(temp, final); err != nil {
		return fmt.Errorf("backup: switch to clone %s/%s step 6: %w", owner, name, err)
	}

	// Step 7: update the stored format and clear verification.
	newPath := syncpkg.PathForFormat(model.FormatClone, owner, name)
	if err := e.repos.SetFormat(repo.ID, model.FormatClone, newPath); err != nil {
		return fmt.Errorf("backup: switch to clone %s/%s step 7: %w", owner, name, err)
	}
	if err := e.repos.SetVerified(repo.ID, nil); err != nil {
		return fmt.Errorf("backup: switch to clone %s/%s step 7: %w", owner, name, err)
	}

	// Step 8: remove the bundle.
	if err := os.RemoveAll(bundlePath); err != nil {
		return fmt.Errorf("backup: switch to clone %s/%s step 8: %w", owner, name, err)
	}
	now := time.Now().UTC()
	if err := e.repos.SetLastBackup(repo.ID, &now); err != nil {
		slog.Error("switch: set last_backup", "repo", repo.Name, "error", err)
	}
	return nil
}

// verifyBundle checks that a bundle file is readable and self-contained.
// git resolves bundle prerequisites against a repository, so the check runs
// inside a throwaway empty bare repo. gh-vault writes bundles with --all from a
// complete bare clone, so a valid bundle needs no objects to verify; a bundle
// that depends on prerequisites would restore only partially, and fails here.
func (e *BackupEngine) verifyBundle(ctx context.Context, path string) error {
	tmp, err := os.MkdirTemp("", "gh-vault-verify-")
	if err != nil {
		return fmt.Errorf("create temp dir: %w", err)
	}
	defer func() {
		if err := os.RemoveAll(tmp); err != nil {
			slog.Warn("backup: remove temp verify dir", "path", tmp, "error", err)
		}
	}()

	if err := e.runGit(ctx, []string{"init", "--bare", "--quiet", tmp}, ""); err != nil {
		return fmt.Errorf("init temp repo: %w", err)
	}
	if err := e.runGit(ctx, []string{"bundle", "verify", path}, tmp); err != nil {
		return fmt.Errorf("bundle verify: %w", err)
	}
	return nil
}

// Verify checks the integrity of a clone or bundle and records the timestamp.
func (e *BackupEngine) Verify(ctx context.Context, repo model.Repo) error {
	var path string
	switch repo.Format {
	case model.FormatClone:
		path = e.activePath(repo.Owner, repo.Name)
	case model.FormatBundle:
		path = e.archivedPath(repo.Owner, repo.Name)
	default:
		return fmt.Errorf("backup: verify %s/%s: unknown format %q", repo.Owner, repo.Name, repo.Format)
	}

	switch repo.Format {
	case model.FormatClone:
		if err := e.runGit(ctx, []string{"--git-dir=" + path, "fsck", "--no-dangling"}, ""); err != nil {
			return fmt.Errorf("backup: verify %s/%s: %w", repo.Owner, repo.Name, err)
		}
	case model.FormatBundle:
		if err := e.verifyBundle(ctx, path); err != nil {
			return fmt.Errorf("backup: verify %s/%s: %w", repo.Owner, repo.Name, err)
		}
	}

	now := time.Now().UTC()
	if err := e.repos.SetVerified(repo.ID, &now); err != nil {
		return fmt.Errorf("backup: verify %s/%s: set verified: %w", repo.Owner, repo.Name, err)
	}
	if err := e.repos.SetLastBackup(repo.ID, &now); err != nil {
		return fmt.Errorf("backup: verify %s/%s: set last backup: %w", repo.Owner, repo.Name, err)
	}
	return nil
}

// MoveBackup renames an existing backup from oldOwner/oldName to
// newOwner/newName after a GitHub rename or transfer, so the backup is not
// orphaned at a stale path and the next backup does not re-clone from scratch.
// For clones it also repoints the origin remote at the new repository, since
// GitHub redirects for old names can break when a new repo takes the old name.
// A missing source is not an error: nothing was backed up yet, and the next
// backup will clone directly at the new path.
func (e *BackupEngine) MoveBackup(ctx context.Context, oldOwner, oldName, newOwner, newName string, format model.RepoFormat) error {
	var oldPath, newPath string
	switch format {
	case model.FormatBundle:
		oldPath = e.archivedPath(oldOwner, oldName)
		newPath = e.archivedPath(newOwner, newName)
	case model.FormatClone:
		oldPath = e.activePath(oldOwner, oldName)
		newPath = e.activePath(newOwner, newName)
	default:
		return fmt.Errorf("backup: move %s/%s: unknown format %q", oldOwner, oldName, format)
	}

	if _, err := os.Stat(oldPath); err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("backup: move %s/%s: stat %s: %w", oldOwner, oldName, oldPath, err)
	}
	if _, err := os.Stat(newPath); err == nil {
		return fmt.Errorf("backup: move %s/%s: target %s already exists", oldOwner, oldName, newPath)
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("backup: move %s/%s: stat %s: %w", oldOwner, oldName, newPath, err)
	}

	if err := os.MkdirAll(filepath.Dir(newPath), 0755); err != nil {
		return fmt.Errorf("backup: move %s/%s: create dir: %w", oldOwner, oldName, err)
	}
	if err := os.Rename(oldPath, newPath); err != nil {
		return fmt.Errorf("backup: move %s/%s: rename: %w", oldOwner, oldName, err)
	}

	if format == model.FormatClone {
		// Repoint origin at the new repository name, keeping the stored URL
		// credential-free.
		if err := e.ensureOrigin(ctx, newPath, newOwner, newName); err != nil {
			return fmt.Errorf("backup: move %s/%s: set origin: %w", oldOwner, oldName, err)
		}
	}

	slog.Info("backup: moved backup to renamed path", "old", oldOwner+"/"+oldName, "new", newOwner+"/"+newName)
	return nil
}

// RemoveLocal deletes the local backup files for a repository. It removes both
// the clone and bundle paths so leftovers from earlier format switches are
// cleaned up as well. Missing paths are not an error.
func (e *BackupEngine) RemoveLocal(owner, name string) error {
	var firstErr error
	for _, path := range []string{e.activePath(owner, name), e.archivedPath(owner, name)} {
		if err := os.RemoveAll(path); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return fmt.Errorf("backup: remove local %s/%s: %w", owner, name, firstErr)
	}
	slog.Info("backup: removed local files", "owner", owner, "name", name)
	return nil
}

// scrubStoredToken removes a token that an earlier version wrote into a
// clone's remote URL. Git stores remote URLs verbatim, so every clone used to
// carry a live credential in plaintext. Reading the config first means an
// already-clean clone costs one file read and no git process.
func (e *BackupEngine) scrubStoredToken(ctx context.Context, repo model.Repo) {
	path := e.activePath(repo.Owner, repo.Name)
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return
	}
	cfg, err := os.ReadFile(filepath.Join(path, "config"))
	if err != nil {
		return
	}
	if !strings.Contains(string(cfg), "x-access-token:") {
		return
	}
	if err := e.ensureOrigin(ctx, path, repo.Owner, repo.Name); err != nil {
		slog.Warn("backup: remove stored credential failed", "owner", repo.Owner, "name", repo.Name, "error", err)
		return
	}
	slog.Info("backup: removed stored credential from clone config", "owner", repo.Owner, "name", repo.Name)
}

// removeRedundantClone deletes a bare clone left next to a bundle by an earlier
// version. A bundle-format repo keeps one copy, so the clone is only removed
// after the bundle passes verification: a bad bundle must never become the sole
// surviving copy.
func (e *BackupEngine) removeRedundantClone(ctx context.Context, repo model.Repo) {
	if repo.Format != model.FormatBundle {
		return
	}

	clone := e.activePath(repo.Owner, repo.Name)
	info, err := os.Stat(clone)
	if err != nil || !info.IsDir() {
		return
	}

	bundlePath := e.archivedPath(repo.Owner, repo.Name)
	bundleInfo, err := os.Stat(bundlePath)
	if err != nil || bundleInfo.IsDir() || bundleInfo.Size() == 0 {
		// No usable bundle: the clone may be the only copy.
		return
	}

	if err := e.verifyBundle(ctx, bundlePath); err != nil {
		slog.Warn("backup: keeping redundant clone, bundle failed verification", "owner", repo.Owner, "name", repo.Name, "error", err)
		return
	}

	if err := os.RemoveAll(clone); err != nil {
		slog.Warn("backup: remove redundant clone failed", "owner", repo.Owner, "name", repo.Name, "error", err)
		return
	}
	slog.Info("backup: removed redundant clone for bundle repo", "owner", repo.Owner, "name", repo.Name)
}

// Reconcile resets stored backup state for repos whose backup files are
// missing from disk — e.g. after a dataset restore, manual deletion, or disk
// loss. It clones nothing: the next backup run re-creates missing copies
// (Backup always copies when the file is absent), and the dashboard reflects
// the true on-disk state in the meantime instead of stale dates. It also
// scrubs credentials left in clone configs by earlier versions and deletes
// clones that a verified bundle has made redundant.
func (e *BackupEngine) Reconcile(ctx context.Context) error {
	repos, err := e.repos.List()
	if err != nil {
		return fmt.Errorf("backup: reconcile: list repos: %w", err)
	}

	var missing int
	for _, repo := range repos {
		if err := ctx.Err(); err != nil {
			return err
		}

		// Independent of backups: drop a clone that a verified bundle has made
		// redundant, then make sure no clone keeps a token in its config, even
		// for repos that are skipped every night.
		e.removeRedundantClone(ctx, repo)
		e.scrubStoredToken(ctx, repo)

		// Only rows that claim a copy exists need checking; repos never backed
		// up are already truthful.
		if repo.LastBackup == nil && repo.VerifiedAt == nil {
			continue
		}

		var path string
		switch repo.Format {
		case model.FormatClone:
			path = e.activePath(repo.Owner, repo.Name)
		case model.FormatBundle:
			path = e.archivedPath(repo.Owner, repo.Name)
		default:
			continue
		}

		if _, err := os.Stat(path); err == nil {
			continue
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("backup: reconcile %s/%s: stat %s: %w", repo.Owner, repo.Name, path, err)
		}

		if err := e.repos.SetLastBackup(repo.ID, nil); err != nil {
			slog.Error("backup: reconcile: reset last_backup", "owner", repo.Owner, "name", repo.Name, "error", err)
			continue
		}
		if err := e.repos.SetVerified(repo.ID, nil); err != nil {
			slog.Error("backup: reconcile: reset verified_at", "owner", repo.Owner, "name", repo.Name, "error", err)
		}
		missing++
	}

	if missing > 0 {
		slog.Warn("backup: reconcile reset state for repos with missing files; next backup run re-clones them", "missing", missing, "repos", len(repos))
	}
	return nil
}
