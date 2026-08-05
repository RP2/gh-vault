# gh-vault

Self-hosted GitHub backup tool. Runs in Docker. Backs up your repositories on a schedule. Gives you a web dashboard to manage them.

## What It Does

gh-vault clones your GitHub repositories and stores them locally. Active projects get full bare clones. Projects you want to take offline get git bundles. You can archive or delete repos on GitHub from the dashboard.

The tool runs on a cron schedule inside a Docker container. It syncs with the GitHub API, backs up changed repos, and logs every action.

## Features

- **Scheduled backups.** Sync and backup run on cron. Default sync runs monthly. Backup runs daily.
- **Web dashboard.** View all repos, trigger backups, switch formats, archive or delete. Built with htmx and Tailwind CSS. No JavaScript framework.
- **Two backup formats.** Bare clones for active repos. Git bundles for archived repos. Switch between them from the dashboard.
- **GitHub archive and delete.** Archive a repo on GitHub to make it read-only. Delete it when you no longer need it. Both actions require a verified backup.
- **Auto-archive.** Enable per repo. The tool archives repos on GitHub after a set number of days with no push activity.
- **Encrypted token storage.** Your GitHub token is stored AES-256-GCM encrypted in SQLite. The encryption key is the only secret you need.
- **Activity log.** Every backup, archive, delete, and sync action is logged. Logs rotate based on your retention setting.
- **Single-user auth.** Session-based login with bcrypt-hashed password. First-run wizard sets up your account.
- **Dry-run mode.** Test the workflow without making changes on GitHub. Local operations still run.

## Tech Stack

| Layer | Choice |
|---|---|
| Language | Go 1.26 |
| GitHub API | `google/go-github/v69` |
| Database | SQLite (pure Go, no CGO) |
| Web server | `net/http` + chi router |
| UI | Go templates + htmx + Tailwind CSS |
| Git operations | `git` CLI via `os/exec` |
| Scheduling | `robfig/cron/v3` |

## Quick Start

### 1. Generate an encryption key

```bash
openssl rand -base64 32 > ./encryption_key
chmod 600 ./encryption_key
```

### 2. Create a docker-compose.yml

```yaml
services:
  gh-vault:
    build: .
    container_name: gh-vault
    restart: unless-stopped
    ports:
      - "8090:8090"
    volumes:
      - /path/to/config:/config
      - /path/to/backups:/backups
    environment:
      - GHVAULT_PORT=8090
    secrets:
      - encryption_key
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8090/healthz"]
      interval: 30s
      timeout: 5s
      retries: 3

secrets:
  encryption_key:
    file: ./encryption_key
```

### 3. Start the container

```bash
docker compose up -d
```

### 4. Open the dashboard

Go to `http://localhost:8090`. The first-run wizard asks you to create a username and password. Then it asks for your GitHub token.

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `GHVAULT_ENCRYPTION_KEY` | Yes | — | 32 bytes, base64. AES-256 key for encrypting the GitHub token. |
| `GHVAULT_PORT` | No | `8090` | Port the web dashboard listens on. |
| `GHVAULT_BACKUP_DIR` | No | `/backups` | Container path for git clones and bundles. |
| `GHVAULT_DATA_DIR` | No | `/config` | Container path for the SQLite database and encrypted secrets. |
| `GHVAULT_BASE_URL` | No | `http://localhost:8090` | Public URL. If it uses HTTPS, session cookies get the Secure flag. |

## Volume Layout

| Container Path | Purpose |
|---|---|
| `/config` | SQLite database, encrypted secrets, app state. Small. Churns frequently. |
| `/backups` | Git clones (`active/`), git bundles (`archived/`). Large. Append-mostly. |

Keep these on separate datasets. This lets you snapshot and retain them independently.

## Backup Formats

**Bare clone** (`active/owner/name.git`): A full mirror of the repository. Supports incremental `fetch`. This is the default for new repos.

**Git bundle** (`archived/owner/name.bundle`): A single file containing the full repository history. Good for offline storage. Use the dashboard to switch a repo from clone to bundle.

## GitHub Token

gh-vault stores your GitHub token encrypted in SQLite. You set it through the web dashboard at `/settings`.

**Required scopes:**
- Classic PAT: `repo`
- Fine-grained PAT: `contents:read` + `metadata:read`

**Token rotation:** Go to Settings and enter a new token. The change takes effect immediately. No container restart needed.

**If you lose the encryption key:** You must re-enter your GitHub token. The encrypted data cannot be recovered without the key.

## Scheduler Jobs

| Job | Schedule | What It Does |
|---|---|---|
| sync | Configurable (default: monthly) | Fetches repo list from GitHub. Detects new, renamed, transferred, and deleted repos. |
| backup | Daily at 02:00 | Backs up all active repos. Up to 3 concurrent. |
| auto-archive | Daily at 03:00 | Archives repos on GitHub that match your criteria. |
| verify | Weekly (Sunday 04:00) | Runs `git fsck` on clones and `git bundle verify` on bundles. |
| log_cleanup | Daily at 05:00 | Deletes logs older than your retention setting. |
| session_cleanup | Daily at 05:30 | Deletes expired sessions. |

## Restore Procedure

1. Stop the container: `docker compose down`
2. Restore the `/config` dataset (database and encrypted secrets).
3. Make sure the encryption key file is in place.
4. Start the container: `docker compose up -d`
5. Optionally restore the `/backups` dataset.
6. Run a sync from the dashboard to reconcile. The syncer detects missing backup paths and re-clones from GitHub.

## Building from Source

```bash
go build -o gh-vault .
```

Requires Go 1.26+ and `git` in your PATH.

## Running Tests

```bash
go test ./...
```

Tests use temporary directories for SQLite databases and real git commands for backup tests. Tests skip if `git` is not in your PATH.

## License

See [LICENSE](LICENSE).
