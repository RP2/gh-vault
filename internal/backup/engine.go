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

// Engine performs backup operations on a repository.
type Engine interface {
	CloneRepo(ctx context.Context, repo model.Repo) error
	FetchRepo(ctx context.Context, repo model.Repo) error
	CreateBundle(ctx context.Context, repo model.Repo) error
	SwitchToBundle(ctx context.Context, repo model.Repo) error
	SwitchToClone(ctx context.Context, repo model.Repo) error
	Verify(ctx context.Context, repo model.Repo) error
	RemoveLocal(owner, name string) error
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

// authURL returns an authenticated GitHub remote URL for the repository.
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

// CloneRepo clones a repository as a bare repo or fetches it if it already
// exists.
func (e *BackupEngine) CloneRepo(ctx context.Context, repo model.Repo) error {
	url, err := e.authURL(ctx, repo.Owner, repo.Name)
	if err != nil {
		return fmt.Errorf("backup: clone %s/%s: %w", repo.Owner, repo.Name, err)
	}

	target := e.activePath(repo.Owner, repo.Name)
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("backup: clone %s/%s: create dir: %w", repo.Owner, repo.Name, err)
	}

	info, err := os.Stat(target)
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("backup: clone %s/%s: target exists but is not a directory", repo.Owner, repo.Name)
		}
		// Bare repos store HEAD directly in the directory; a missing HEAD means
		// the directory is stale and should be recloned.
		if _, err := os.Stat(filepath.Join(target, "HEAD")); err != nil {
			if !os.IsNotExist(err) {
				return fmt.Errorf("backup: clone %s/%s: stat HEAD: %w", repo.Owner, repo.Name, err)
			}
			slog.Warn("backup: target directory missing HEAD, removing stale clone", "owner", repo.Owner, "name", repo.Name)
			if err := os.RemoveAll(target); err != nil {
				return fmt.Errorf("backup: clone %s/%s: remove stale clone: %w", repo.Owner, repo.Name, err)
			}
		} else {
			if err := e.runGit(ctx, []string{"--git-dir=" + target, "fetch", "--prune", "origin"}, ""); err != nil {
				slog.Error("backup: fetch failed", "owner", repo.Owner, "name", repo.Name, "error", err)
				return fmt.Errorf("backup: clone %s/%s: %w", repo.Owner, repo.Name, err)
			}
			slog.Info("backup: fetch succeeded", "owner", repo.Owner, "name", repo.Name)
			if err := e.repos.SetVerified(repo.ID, nil); err != nil {
				slog.Error("backup: clear verified_at", "repo", repo.Name, "error", err)
			}
			return nil
		}
	} else if !os.IsNotExist(err) {
		return fmt.Errorf("backup: clone %s/%s: stat target: %w", repo.Owner, repo.Name, err)
	}

	if err := e.runGit(ctx, []string{"clone", "--bare", url, target}, ""); err != nil {
		slog.Error("backup: clone failed", "owner", repo.Owner, "name", repo.Name, "error", err)
		return fmt.Errorf("backup: clone %s/%s: %w", repo.Owner, repo.Name, err)
	}
	slog.Info("backup: clone succeeded", "owner", repo.Owner, "name", repo.Name)
	if err := e.repos.SetVerified(repo.ID, nil); err != nil {
		slog.Error("backup: clear verified_at", "repo", repo.Name, "error", err)
	}
	return nil
}

// FetchRepo fetches the latest state into an existing bare clone.
func (e *BackupEngine) FetchRepo(ctx context.Context, repo model.Repo) error {
	if _, err := e.tokenProvider.GetToken(ctx); err != nil {
		return fmt.Errorf("backup: fetch %s/%s: %w", repo.Owner, repo.Name, err)
	}

	target := e.activePath(repo.Owner, repo.Name)
	info, err := os.Stat(target)
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("backup: fetch %s/%s: repo not found", repo.Owner, repo.Name)
		}
		return fmt.Errorf("backup: fetch %s/%s: stat target: %w", repo.Owner, repo.Name, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("backup: fetch %s/%s: target is not a directory", repo.Owner, repo.Name)
	}

	if err := e.runGit(ctx, []string{"--git-dir=" + target, "fetch", "--prune", "origin"}, ""); err != nil {
		return fmt.Errorf("backup: fetch %s/%s: %w", repo.Owner, repo.Name, err)
	}
	return nil
}

// createBundle writes a bundle file for the given repository.
func (e *BackupEngine) createBundle(ctx context.Context, repo model.Repo, target string) error {
	source := e.activePath(repo.Owner, repo.Name)
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

// CreateBundle creates a bundle from the active bare clone.
func (e *BackupEngine) CreateBundle(ctx context.Context, repo model.Repo) error {
	target := e.archivedPath(repo.Owner, repo.Name)
	if err := e.createBundle(ctx, repo, target); err != nil {
		return fmt.Errorf("backup: create bundle %s/%s: %w", repo.Owner, repo.Name, err)
	}
	slog.Info("backup: bundle created", "owner", repo.Owner, "name", repo.Name)
	if err := e.repos.SetVerified(repo.ID, nil); err != nil {
		slog.Error("backup: clear verified_at", "repo", repo.Name, "error", err)
	}
	return nil
}

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
	if err := e.createBundle(ctx, repo, temp); err != nil {
		return fmt.Errorf("backup: switch to bundle %s/%s step 1: %w", owner, name, err)
	}

	// Step 2: verify the bundle (must run inside the source repo).
	if err := e.runGit(ctx, []string{"bundle", "verify", temp}, source); err != nil {
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

	// Step 3: point origin at the live GitHub remote.
	if err := e.runGit(ctx, []string{"--git-dir=" + temp, "remote", "set-url", "origin", authURL}, ""); err != nil {
		return fmt.Errorf("backup: switch to clone %s/%s step 3: %w", owner, name, err)
	}

	// Step 4: fetch the latest refs from GitHub.
	if err := e.runGit(ctx, []string{"--git-dir=" + temp, "fetch", "origin", "--prune"}, ""); err != nil {
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
		if err := e.runGit(ctx, []string{"bundle", "verify", path}, ""); err != nil {
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
		url, err := e.authURL(ctx, newOwner, newName)
		if err != nil {
			return fmt.Errorf("backup: move %s/%s: %w", oldOwner, oldName, err)
		}
		if err := e.runGit(ctx, []string{"--git-dir=" + newPath, "remote", "set-url", "origin", url}, ""); err != nil {
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
