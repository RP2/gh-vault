# gh-vault

Self-hosted GitHub backup tool. Runs in Docker. Backs up your repos on a schedule. Includes a web dashboard.

## What It Does

gh-vault clones your GitHub repos and stores them locally. It runs inside a Docker container on a cron schedule. It syncs with the GitHub API, saves changed repos, and logs every action.

## Features

- **Scheduled backups.** Sync and backup run on cron. Default sync runs daily at 23:00 (container local time). Backup follows at 23:30.
- **Web dashboard.** View all repos, trigger backups, switch formats. Built with htmx. No JavaScript framework.
- **Two backup formats.** Bare clones for active repos. Git bundles for offline storage. Switch between them from the dashboard.
- **HTTPS by default.** gh-vault generates a self-signed certificate on first start and serves over HTTPS. Set `DISABLE_TLS=true` for plain HTTP behind a reverse proxy.
- **Encrypted token storage.** gh-vault encrypts your GitHub token with AES-256-GCM in SQLite. The encryption key is managed automatically.
- **Password change.** Change your password from the settings page. All sessions are invalidated on change.
- **Activity log.** gh-vault logs every backup and sync action. Logs rotate based on your retention setting.
- **Job status on the dashboard.** The last sync and backup outcomes are shown at the top of the dashboard, including failure reasons.
- **Removed-repo handling.** Repos deleted from GitHub keep their local copy — that is the point of a backup. The dashboard lists them and lets you delete the local copy when you no longer want it.
- **Single-user auth.** Session-based login with bcrypt-hashed password. First-run wizard creates your account.

## Tech Stack

| Layer | Choice |
|---|---|
| Language | Go 1.26 |
| GitHub API | `google/go-github/v69` |
| Database | SQLite (pure Go, no CGO) |
| Web server | `net/http` + chi router |
| UI | Go templates + htmx |
| Git operations | `git` CLI via `os/exec` |
| Scheduling | `robfig/cron/v3` |
| TLS | ECDSA P-256 self-signed certificate (auto-generated) |

## Quick Start

### 1. Create a docker-compose.yml

```yaml
services:
  gh-vault:
    image: ghcr.io/rp2/gh-vault:main
    container_name: gh-vault
    restart: unless-stopped
    read_only: true
    cap_drop:
      - ALL
    security_opt:
      - no-new-privileges:true
    ports:
      - 8090:8090
    volumes:
      - /path/to/config:/config
      - /path/to/backups:/backups
    environment:
      - PORT=8090
      # - TZ=America/New_York  # Local time for cron schedules and displayed timestamps (default: UTC)
    logging:
      # Cap container logs so they cannot grow without bound on the host.
      driver: json-file
      options:
        max-size: "10m"
        max-file: "3"
```

The `:main` tag is rebuilt on every push. For a deployment that only changes when you decide, pin a version or digest tag instead — both are shown on the package page (e.g. `image: ghcr.io/rp2/gh-vault:main@sha256:0123abcd…`).

### 2. Start the container

```bash
docker compose up -d
```

### 3. Open the dashboard

Go to `https://localhost:8090`. Your browser shows a warning about the self-signed certificate. Accept it to proceed.

The first-run wizard asks you to create a username and password. Then it asks for your GitHub token.

## Environment Variables

| Variable | Required | Default | Description |
|---|---|---|---|
| `ENCRYPTION_KEY` | No | auto-generated | 32 bytes, base64. AES-256 key for encrypting the GitHub token. |
| `PORT` | No | `8090` | Port the web dashboard listens on. |
| `BACKUP_DIR` | No | `/backups` | Container path for git clones and bundles. Preset in the image to the standard mount point; override only for non-Docker runs or custom volume paths. |
| `DATA_DIR` | No | `/config` | Container path for the SQLite database and encrypted secrets. Preset in the image; same override rule. |
| `BASE_URL` | No | `http://localhost:8090` | Public URL for redirect targets. |
| `DISABLE_TLS` | No | `false` | Set to `true` for plain HTTP. Use this when a reverse proxy handles TLS. |
| `TZ` | No | `UTC` | Timezone for cron schedules and displayed timestamps, e.g. `America/New_York`. The image includes `tzdata`. |

## Timezone

Cron schedules and displayed timestamps follow the container's local time. Set `TZ` to an IANA timezone name (`Area/City` format, e.g. `America/New_York`, `Europe/Berlin`). Without it, everything runs on UTC.

**In docker-compose.yml:**

```yaml
    environment:
      - TZ=America/New_York
```

**In a Dockerfile** — if you build your own image instead of using an env file or compose environment:

```dockerfile
FROM ghcr.io/RP2/gh-vault:main
ENV TZ=America/New_York
```

## Volume Layout

| Container Path | Purpose |
|---|---|
| `/config` | SQLite database, encrypted secrets, TLS certificate, app state. Small. Changes often. |
| `/backups` | Git clones (`active/`), git bundles (`archived/`). Large. Append-mostly. |

Keep these on separate datasets. This lets you snapshot and retain them independently.

## Backup Formats

**Bare clone** (`active/owner/name.git`): A full mirror of the repo. Supports incremental `fetch`. This is the default for new repos.

**Git bundle** (`archived/owner/name.bundle`): A single file containing the full repo history. Good for offline storage. Use the dashboard to switch a repo from clone to bundle.

## GitHub Token

gh-vault encrypts your GitHub token and stores it in SQLite. You set the token through the web dashboard at `/settings`.

**Required scopes:**
- Classic PAT: `repo`
- Fine-grained PAT: `contents:read` + `metadata:read`

**Token rotation:** Go to Settings and enter a new token. The change applies immediately. No container restart needed.

**If you lose the encryption key:** You must re-enter your GitHub token. The encrypted data cannot be recovered without the key. The key lives in the `/config` dataset. Save this dataset to preserve the key.

## Encryption Key Lifecycle

The encryption key protects your GitHub token at rest.

**How it is generated:** On first startup, if no key is configured, gh-vault generates a random 32-byte key and writes it to `{DATA_DIR}/encryption_key`. This file has `0600` permissions (owner read/write only).

If a database already exists and no key is found, gh-vault refuses to auto-generate. This prevents silent key rotation. You must set `ENCRYPTION_KEY` explicitly.

**Where it lives:** The key lives inside the `/config` Docker volume. If you copy this volume, you copy the key. If you lose this volume without `ENCRYPTION_KEY` set and no secret mounted at `/run/secrets/encryption_key`, your encrypted GitHub token is unrecoverable.

**What to do:**
- **Back up `/config` regularly.**
- **Set `ENCRYPTION_KEY` in your environment** to keep the key after volume loss. The value must be exactly 32 bytes after base64 decoding. Generate one with: `openssl rand -base64 32`
- **Do not share the key.** Anyone with the key can decrypt your GitHub token.

**Key co-location warning:** The encryption key and the encrypted database both live in `/config`. A process with filesystem access to this directory can read both files. For stronger protection, set `ENCRYPTION_KEY` via Docker secret at `/run/secrets/encryption_key` and remove the auto-generated file.

## TLS

gh-vault generates a self-signed ECDSA P-256 certificate on first startup. The certificate is valid for 1 year and includes SANs for `localhost`, `127.0.0.1`, `::1`, and the container hostname.

The certificate files live at `{DATA_DIR}/tls/cert.pem` and `{DATA_DIR}/tls/key.pem`. gh-vault validates these files on every startup. If the files are corrupt, missing, or the hostname changes, gh-vault regenerates them automatically.

**Behind a reverse proxy:** Set `DISABLE_TLS=true` and point your proxy at `http://gh-vault:8090`.

## Scheduler Jobs

Sync calls the GitHub API to reconcile your local repo list. It adds new repos, detects renames and transfers, and marks deleted repos. It does not download git data.

Backup runs `git clone` or `git fetch` to download repository contents. It runs on all repos with backup enabled.

All times are container-local. Set `TZ` to your timezone so schedules and timestamps match your clock; without it everything runs on UTC.

| Job | Schedule | What It Does |
|---|---|---|
| sync | Configurable (default: daily 23:00) | Fetches repo list from GitHub. Detects new, renamed, transferred, and deleted repos. |
| backup | Daily at 23:30 | Clones or fetches all repos with backup enabled. Up to 3 concurrent. |
| verify | Weekly (Sunday 04:00) | Runs `git fsck` on clones and `git bundle verify` on bundles. |
| log_cleanup | Daily at 05:00 | Deletes logs older than your retention setting. |
| session_cleanup | Daily at 04:45 | Deletes expired sessions. |

The sync schedule is configurable from the Settings page. Existing installs keep whatever schedule is stored in their database until changed there.

## Monitoring

There is no in-container healthcheck; monitor the container externally instead (logs via Dozzle, uptime via any checker). The `/healthz` endpoint returns 200 without authentication and is excluded from request logging, so a frequent monitor there costs no disk writes: `https://your-host:8090/healthz`.

## Security

- Container runs as non-root (UID 568) with read-only filesystem and all capabilities dropped.
- Passwords are hashed with bcrypt (cost 12). The login endpoint uses constant-time comparison to prevent timing attacks.
- Session cookies use `HttpOnly`, `SameSite=Strict`, and `Secure` flags. Sessions expire after 24 hours.
- CSRF protection on all state-changing endpoints, including the setup wizard.
- Rate limiting on login and password change (5 attempts per 15 minutes per IP).
- Session IDs are stored as SHA-256 hashes in the database.
- All sessions are invalidated when you change your password.
- Security headers: `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Strict-Transport-Security`, `Content-Security-Policy`.

**Known limitations:**
- The encryption key and encrypted database are co-located in `/config`. A process with filesystem access can read both. Use a Docker secret for the key in production.
- Rate limiting is in-memory and resets on container restart.
- Rate limiting uses the connecting IP directly. When behind a reverse proxy, all clients may share the proxy's IP. Configure rate limiting at the proxy layer instead.

## Restore Procedure

1. Stop the container: `docker compose down`
2. Restore the `/config` dataset. This includes the database, encrypted secrets, and encryption key.
3. Start the container: `docker compose up -d`
4. Optionally restore the `/backups` dataset.
5. Start the container. On startup and before every backup run, gh-vault compares stored state against disk, clears stale `last backup` entries, and the next backup run re-clones anything missing from GitHub.

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
