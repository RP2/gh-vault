# gh-vault

Self-hosted GitHub backup tool. Runs in Docker. Backs up your repositories on a schedule. Gives you a web dashboard to manage them.

## What It Does

gh-vault clones your GitHub repositories and stores them locally. Active projects get full bare clones. Projects you want to take offline get git bundles.

The tool runs on a cron schedule inside a Docker container. It syncs with the GitHub API, backs up changed repos, and logs every action.

## Features

- **Scheduled backups.** Sync and backup run on cron. Default sync runs monthly. Backup runs daily.
- **Web dashboard.** View all repos, trigger backups, switch formats. Built with htmx and Tailwind CSS. No JavaScript framework.
- **Two backup formats.** Bare clones for active repos. Git bundles for offline storage. Switch between them from the dashboard.
- **Encrypted token storage.** Your GitHub token is stored AES-256-GCM encrypted in SQLite. The app generates and manages its own encryption key.
- **Activity log.** Every backup and sync action is logged. Logs rotate based on your retention setting.
- **Single-user auth.** Session-based login with bcrypt-hashed password. First-run wizard sets up your account.

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

### 1. Create a docker-compose.yml

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
      - PORT=8090
    healthcheck:
      test: ["CMD", "wget", "-qO-", "http://localhost:8090/healthz"]
      interval: 30s
      timeout: 5s
      retries: 3
```

### 2. Start the container

```bash
docker compose up -d
```

### 3. Open the dashboard

Go to `http://localhost:8090`. The first-run wizard asks you to create a username and password. Then it asks for your GitHub token.

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `ENCRYPTION_KEY` | No | auto-generated | 32 bytes, base64. AES-256 key for encrypting the GitHub token. If not set, the app generates one on first run and saves it to the data directory. |
| `PORT` | No | `8090` | Port the web dashboard listens on. |
| `BACKUP_DIR` | No | `/backups` | Container path for git clones and bundles. |
| `DATA_DIR` | No | `/config` | Container path for the SQLite database and encrypted secrets. |
| `BASE_URL` | No | `http://localhost:8090` | Public URL. If it uses HTTPS, session cookies get the Secure flag. |

## Volume Layout

| Container Path | Purpose |
|---|---|
| `/config` | SQLite database, encrypted secrets, encryption key, app state. Small. Churns frequently. |
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

**If you lose the encryption key:** You must re-enter your GitHub token. The encrypted data cannot be recovered without the key. The key is stored in the `/config` dataset. Back up this dataset to preserve the key.

## Scheduler Jobs

Sync calls the GitHub API to reconcile your local repo list. It adds new repos, detects renames and transfers, and marks deleted repos. It does not download git data.

Backup runs `git clone` or `git fetch` to download repository contents. It runs on all repos with backup enabled.

| Job | Schedule | What It Does |
|---|---|---|
| sync | Configurable (default: monthly) | Fetches repo list from GitHub. Detects new, renamed, transferred, and deleted repos. |
| backup | Daily at 02:00 | Clones or fetches all repos with backup enabled. Up to 3 concurrent. |
| verify | Weekly (Sunday 04:00) | Runs `git fsck` on clones and `git bundle verify` on bundles. |
| log_cleanup | Daily at 05:00 | Deletes logs older than your retention setting. |
| session_cleanup | Daily at 05:30 | Deletes expired sessions. |

## Restore Procedure

1. Stop the container: `docker compose down`
2. Restore the `/config` dataset. This includes the database, encrypted secrets, and encryption key.
3. Start the container: `docker compose up -d`
4. Optionally restore the `/backups` dataset.
5. Run a sync from the dashboard to reconcile. The syncer detects missing backup paths and re-clones from GitHub.

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
