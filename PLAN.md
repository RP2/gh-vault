# gh-vault — Self-Hosted GitHub Backup Tool

A lean Go application that runs in Docker on TrueNAS, backs up your GitHub repos on a cron schedule, and provides a web dashboard to manage active vs archived repos. Full clones for active projects, git bundles for projects you want to take offline, with optional archive-then-delete workflow via the GitHub API.

## Tech Stack

| Layer       | Choice                                          | Why                                                                                                                                   |
| ----------- | ----------------------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| Language    | **Go 1.26** (pinned)                            | Single static binary, ~15MB Alpine Docker image, no CGO                                                                               |
| GitHub API  | `google/go-github/v69`                          | Official client. Rate limit handling is manual (check `RateLimits`, respect `Retry-After`)                                            |
| Scheduling  | `robfig/cron/v3`                                | Standard cron syntax, zero deps, battle-tested                                                                                        |
| Database    | `modernc.org/sqlite` (pure Go)                  | Embedded SQLite, no CGO, WAL mode                                                                                                     |
| Web server  | `net/http` + `go-chi/chi/v5`                    | Stdlib + lightweight router                                                                                                           |
| UI          | **Go templates + htmx + vendored Tailwind CSS** | No build step, no npm, no JS framework. Tailwind CSS is a pre-built `.css` file committed to `internal/web/static/`, NOT the Play CDN |
| Git ops     | `os/exec` -> `git` CLI                          | Reliable, 100% compatible, simple                                                                                                     |
| Auth        | Session-based with bcrypt-hashed credentials    | Single-user, sessions stored in SQLite                                                                                                |
| Logging     | `log/slog` (stdlib)                             | Structured logging, zero deps                                                                                                         |
| Concurrency | `golang.org/x/sync/errgroup`                    | Fan-out backup jobs with error collection                                                                                             |
| Rate limit  | `golang.org/x/time/rate`                        | Login rate limiting (5 attempts/min per IP)                                                                                           |

**Module path**: `github.com/RP2/gh-vault` — this MUST match the actual GitHub repository path. If the repo is created under a different org/user, update `go.mod` and all internal imports accordingly.

## Architecture

```
+---------------------------------------------+
|  gh-vault (Go binary, ~15MB Docker image)   |
|  +----------+ +----------+ +-----------+    |
|  | Web UI   | | Cron     | | GitHub    |    |
|  | (htmx)   | | Scheduler| | API       |    |
|  +----+-----+ +----+-----+ +-----+-----+    |
|       +--------------+--------------+       |
|       |      Core Engine              |       |
|       | - Repo enumeration (sync)    |       |
|       | - Backup (clone/bundle)      |       |
|       | - Archive/delete on GitHub   |       |
|       | - State tracking (store)     |       |
|       +------------------------------+       |
|       |                    |                 |
|  +----+-----------+ +------------------+     |
|  | SQLite DB      | | Secrets (AES-    |     |
|  | (state/logs)   | | 256-GCM)         |     |
|  +----------------+ +------------------+     |
+---------------------------------------------+
         |                  |
         v                  v
    /backups           /config
    (host path)        (host path)
    +-- active/        +-- gh-vault.db
    +-- archived/      +-- encrypted secrets
```

## Domain Model Types

```go
// internal/model/model.go

type Repo struct {
    ID             int64
    GitHubID       int64
    Owner          string
    Name           string
    Format         RepoFormat    // "clone" or "bundle". Syncer MUST set Format: model.FormatClone on new repos (CHECK constraint rejects "")
    BackupPath     *string       // pointer — NULL when no backup exists yet
    GitHubURL      *string       // pointer — nullable SQL column
    Language       *string       // pointer — nullable SQL column
    SizeKB         int64
    LastPush       *time.Time    // nullable — nil if never pushed
    LastBackup     *time.Time    // nullable — nil if never backed up
    VerifiedAt     *time.Time    // nullable — nil if never verified
    GitHubArchived bool
    GitHubDeleted  bool
    AutoArchive    bool
    CreatedAt      time.Time
    UpdatedAt      time.Time
}

type RepoFormat string

const (
    FormatClone  RepoFormat = "clone"
    FormatBundle RepoFormat = "bundle"
)

type LogEntry struct {
    ID        int64
    RepoID    int64
    Action    string    // "backup", "archive", "delete", "switch_format", "sync"
    Status    string    // "success", "error"
    Message   string
    CreatedAt time.Time
}

type Session struct {
    ID         string    // random 32-byte hex
    UserID     int64
    CSRFToken  string
    ExpiresAt  time.Time
    CreatedAt time.Time
}

type User struct {
    ID           int64
    Username     string
    PasswordHash string
    CreatedAt    time.Time
}

type Settings struct {
    CronSchedule      string    // key: "cron_schedule"
    DryRun            bool      // key: "dry_run"
    AutoArchiveDays   int       // key: "auto_archive_days" (0 = disabled)
    LogRetentionDays  int       // key: "log_retention_days"
}
// SettingsStore.GetAll() parses each key to its typed field. Unknown keys ignored.
// Typed setters: SetBool(key, bool), SetInt(key, int) — serialize back to TEXT for storage.
```

## Data Model (SQLite)

All tables use `PRAGMA foreign_keys = ON` (set on every connection open).
Schema versioning uses `PRAGMA user_version` (atomic, no separate table needed).

```sql
-- Migration v1 (initial schema, applied when user_version < 1)

CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE TABLE IF NOT EXISTS repos (
    id              INTEGER PRIMARY KEY,
    github_id       INTEGER NOT NULL UNIQUE,
    owner           TEXT NOT NULL,
    name            TEXT NOT NULL,
    format          TEXT NOT NULL DEFAULT 'clone' CHECK(format IN ('clone', 'bundle')),
    backup_path     TEXT,
    github_url      TEXT,
    language        TEXT,
    size_kb         INTEGER DEFAULT 0,
    last_push       DATETIME,
    last_backup     DATETIME,
    verified_at     DATETIME,
    github_archived BOOLEAN NOT NULL DEFAULT FALSE,
    github_deleted   BOOLEAN NOT NULL DEFAULT FALSE,
    auto_archive    BOOLEAN NOT NULL DEFAULT FALSE,
    created_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    updated_at      DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
    UNIQUE(owner, name)
);

CREATE TRIGGER IF NOT EXISTS trg_repos_updated_at AFTER UPDATE ON repos
BEGIN UPDATE repos SET updated_at = CURRENT_TIMESTAMP WHERE id = NEW.id;
END;

-- Note: UNIQUE(owner, name) above already creates an index; no separate idx_repos_owner_name needed.

CREATE TABLE IF NOT EXISTS sessions (
    id          TEXT PRIMARY KEY,
    user_id     INTEGER NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    csrf_token  TEXT NOT NULL,
    expires_at  DATETIME NOT NULL,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

CREATE TABLE IF NOT EXISTS logs (
    id         INTEGER PRIMARY KEY,
    repo_id    INTEGER REFERENCES repos(id) ON DELETE CASCADE,
    action     TEXT NOT NULL,
    status     TEXT NOT NULL CHECK(status IN ('success', 'error')),
    message    TEXT,
    created_at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_logs_created_at ON logs(created_at DESC);

CREATE TABLE IF NOT EXISTS settings (
    key   TEXT PRIMARY KEY,
    value TEXT NOT NULL
);

INSERT OR IGNORE INTO settings(key, value) VALUES ('cron_schedule', '0 3 1 * *');
INSERT OR IGNORE INTO settings(key, value) VALUES ('dry_run', 'false');
INSERT OR IGNORE INTO settings(key, value) VALUES ('auto_archive_days', '0');
INSERT OR IGNORE INTO settings(key, value) VALUES ('log_retention_days', '90');

-- Migration v2 (applied when user_version < 2)

CREATE TABLE IF NOT EXISTS secrets (
    key        TEXT PRIMARY KEY,
    value      BLOB NOT NULL,
    nonce      BLOB NOT NULL,
    created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
    updated_at DATETIME DEFAULT CURRENT_TIMESTAMP
);

-- Add last_validated to secrets for token status tracking (#6)
-- Store as settings key 'token_last_validated' with ISO8601 value — no schema change needed.

PRAGMA user_version = 2;
```

**Migration runner logic:**

1. Read `PRAGMA user_version` → `currentVersion`
2. Define ordered migrations: `[]Migration{{Version: 1, DDL: v1DDL}, {Version: 2, DDL: v2DDL}}`
3. For each migration where `Version > currentVersion`: begin transaction → execute DDL → `PRAGMA user_version = N` → commit
4. All DDL uses `IF NOT EXISTS` / `INSERT OR IGNORE` for idempotency

## Package Interfaces

Every package boundary is defined by an interface. `main.go` wires concrete implementations together via constructor injection.

```go
// internal/store/store.go
type Store interface {
    Repos() RepoStore
    Logs() LogStore
    Sessions() SessionStore
    Settings() SettingsStore
    Users() UserStore
    Secrets() SecretStore
    Close() error
}

type RepoStore interface {
    List() ([]model.Repo, error)
    Get(id int64) (model.Repo, error)
    GetByOwnerName(owner, name string) (model.Repo, error)
    Upsert(r model.Repo) (int64, error)
    SetLastBackup(id int64, at time.Time) error
    SetGitHubMetadata(id int64, sizeKB int64, language, url string, archived bool, lastPush *time.Time) error
    SetOwnerName(id int64, owner, name, url string) error
    SetBackupPath(id int64, path string) error  // called after disk rename (rename/transfer)
    // MarkDeleted = soft-delete (sets github_deleted=TRUE, retains backup on disk and row in DB for audit).
    // Delete = removes from store entirely (removes the DB row, called after backup GC). Only Delete removes the row.
    // DeletePermanent = alias for Delete (removes row entirely).
    MarkDeleted(id int64) error
    Delete(id int64) error
    DeletePermanent(id int64) error
    SetFormat(id int64, f model.RepoFormat, path string) error
    SetVerified(id int64, at *time.Time) error  // accepts *time.Time — pass nil to set verified_at = NULL (e.g. after format switch)
    SetAutoArchive(id int64, enabled bool) error
}

type LogStore interface {
    Create(l model.LogEntry) error
    Recent(limit int) ([]model.LogEntry, error)
    DeleteOlderThan(before time.Time) (int64, error)
}

type SessionStore interface {
    Create(userID int64, ttl time.Duration) (model.Session, error)
    Get(id string) (*model.Session, error)
    Delete(id string) error
    DeleteExpired() (int64, error)
}

type SettingsStore interface {
    Get(key string) (string, error)
    Set(key, value string) error
    GetAll() (model.Settings, error)
}

type UserStore interface {
    Count() (int, error)
    Create(username, passwordHash string) (int64, error)
    GetByUsername(username string) (*model.User, error)
}

type SecretStore interface {
    Get(key string) (value []byte, nonce []byte, err error)
    Set(key string, value []byte, nonce []byte) error
    Delete(key string) error
}
```

```go
// internal/github/tokenprovider.go
type TokenProvider interface {
    GetToken(ctx context.Context) (string, error)
    SetToken(ctx context.Context, token string) error
}
```

Implementation: `internal/github/dbtokenprovider.go` — reads encrypted token from `secrets` table at key `github_token`. AES-256-GCM with random 12-byte nonce per write. Caches decrypted token in-memory with `sync.Mutex`. `SetToken` encrypts + writes to DB + updates cache. Encryption key from `GHVAULT_ENCRYPTION_KEY` env var or Docker secret file — loader base64-decodes in both cases to get raw 32 bytes.

```go
// internal/github/client.go
type Client interface {
    ListOwnedRepos(ctx context.Context) ([]*github.Repository, error)
    GetRepo(ctx context.Context, owner, name string) (*github.Repository, error)
    ArchiveRepo(ctx context.Context, owner, name string) error
    DeleteRepo(ctx context.Context, owner, name string) error
    RateLimitStatus(ctx context.Context) (*github.Rate, error)
}

// Constructor: NewClient(tokenProvider TokenProvider) *Client
// Queries tokenProvider.GetToken(ctx) on every API call, not once at init.
```

```go
// internal/backup/engine.go
type Engine interface {
    CloneRepo(ctx context.Context, repo model.Repo) error
    FetchRepo(ctx context.Context, repo model.Repo) error
    CreateBundle(ctx context.Context, repo model.Repo) error
    SwitchToBundle(ctx context.Context, repo model.Repo) error
    SwitchToClone(ctx context.Context, repo model.Repo) error
    Verify(ctx context.Context, repo model.Repo) error
}

// Constructor: NewEngine(backupDir string, tokenProvider TokenProvider, repos store.RepoStore) *Engine
// All methods call tokenProvider.GetToken(ctx) internally for git auth.
// SwitchToBundle/SwitchToClone/Verify call repos.SetFormat/repos.SetVerified on success.
```

```go
// internal/sync/syncer.go
type Syncer interface {
    SyncRepos(ctx context.Context) (SyncResult, error)
}

type SyncResult struct {
    Added       int
    Updated     int
    Renamed     int
    Transferred int
    Deleted     int
    Unchanged   int
    ErrorCount  int // each error logged via slog.Error as it occurs; count shown on dashboard
}
```

```go
// internal/scheduler/scheduler.go
type Scheduler interface {
    Start() error
    Stop() error
    ReloadCron(expr string) error
    NextRun(jobName string) time.Time
    IsRunning(jobName string) bool
}
```

## Error Handling Pattern

- Each package defines sentinel errors: `var ErrNotFound = errors.New("store: not found")`.
- Wrap with `fmt.Errorf("context: %w", err)` at every boundary.
- Handlers check with `errors.Is(err, store.ErrNotFound)` and map to HTTP status codes.
- No panics. All errors returned and handled.

## Dependency Injection

`main.go` constructs all dependencies via constructor injection:

```go
func main() {
    cfg := config.Load()
    db := store.New(cfg.DataDir + "/gh-vault.db")
    tokenProvider := github.NewDBTokenProvider(db, cfg.EncryptionKey)
    ghClient := github.NewClient(tokenProvider)
    backupEngine := backup.NewEngine(cfg.BackupDir, tokenProvider, db.Repos())
    syncer := sync.NewSyncer(ghClient, db.Repos(), db.Logs())
    archiveWorkflow := workflow.NewArchive(db.Repos(), db.Logs(), db.Settings(), ghClient)
    sched := scheduler.New(db.Settings(), syncer, backupEngine, db.Repos(), db.Logs(), db.Sessions(), archiveWorkflow)
    srv := web.NewServer(cfg, db, backupEngine, syncer, sched, db.Sessions(), tokenProvider, ghClient)
    // ... graceful shutdown
}
```

## Auth & Session Model

- **First-run state machine**:
  - `users` table empty → all routes redirect to `GET /setup`
  - No valid session cookie → redirect to `GET /login`
  - Valid session but no token in `secrets` table → redirect to `GET /settings?reason=token_missing`
  - Valid session + token exists → serve dashboard
- **First-run wizard** (two steps):
  1. Create username + password (`POST /setup` → saves credentials, auto-creates session, redirects to `/settings?reason=token_missing`)
  2. Enter GitHub Personal Access Token at `/settings`. Scope hint: `repo` for classic PAT, `contents:read` + `metadata:read` for fine-grained. `POST /settings/token` validates via GitHub API, encrypts, stores in `secrets` table.
- **Session lifecycle**:
  1. `POST /login` validates credentials, creates `sessions` row, returns `Set-Cookie`
  2. Cookie: `session=<id>; Path=/; HttpOnly; SameSite=Lax; Max-Age=86400`
  3. Add `Secure` flag when `GHVAULT_BASE_URL` is `https://`
  4. `GET /logout` deletes session row, clears cookie
  5. Expired sessions cleaned by daily cron job
- **CSRF**: Every POST form includes `<input type="hidden" name="csrf" value="{{.CSRFToken}}">`. For htmx non-form POSTs, add `hx-headers='{"X-CSRF-Token": "{{.CSRFToken}}"}'` on the container element. Server validates both form field and `X-CSRF-Token` header. Mismatch → 403.
- **Login rate limiter**: 5 attempts/min per IP via `golang.org/x/time/rate`. Excess → 429. Entries evicted after 10min of inactivity (periodic cleanup goroutine) to prevent unbounded memory growth.
- **Token last validated**: stored as `settings` key `token_last_validated` (ISO8601 string). Updated on every successful `POST /settings/token` and on `GET /settings/token-status` when GitHub confirms token still valid.

## Routing

```
State machine (middleware, runs before route matching):
  1. If users count == 0 AND path != /setup → redirect to /setup
  1b. If users count > 0 AND path == /setup → return 404 (setup already complete)
  2. If no valid session AND path not in [/login, /setup, /static/*, /healthz] → redirect to /login
  3. If valid session AND no token in secrets AND path not in [/settings, /settings/token, /settings/token-status, /logout, /static/*, /healthz] → redirect to /settings?reason=token_missing
  4. Otherwise → serve request

Routes:
  GET  /setup                → First-run setup wizard (create username + password)
   POST /setup                → Save credentials, auto-login, redirect to /settings?reason=token_missing
  GET  /login                → Login page
   POST /login                → Authenticate, set session cookie, redirect to / (rate-limited: 5 attempts/min per IP)
  GET  /logout               → Clear session, redirect to /login
  GET  /                     → Dashboard (overview stats)
  GET  /repos                → Repo list with status, format, last backup
   POST /repos/{id}/switch     → Switch format (clone <-> bundle)
   POST /repos/{id}/archive    → Archive on GitHub (set archived: true)
   POST /repos/{id}/delete     → Delete from GitHub (server checks posted name matches repo name)
   DELETE /repos/{id}           → Permanently remove repo from store (requires name confirmation in query param)
   POST /repos/{id}/backup     → Trigger manual backup (async, returns 202 + log entry)
   POST /repos/{id}/auto-archive → Toggle auto_archive flag
   POST /trigger/sync         → Trigger full GitHub sync (async, returns 202)
   POST /trigger/backup       → Trigger all backups (async, returns 202)
  GET  /logs                 → Recent activity log (paginated: `?page=N&size=50`)
  GET  /settings             → Settings page (cron, dry-run, auto_archive_days, token management)
   POST /settings             → Update settings
   POST /settings/token       → Validate + encrypt + store GitHub token, update TokenProvider cache
  GET  /settings/token-status → JSON: token set (bool), last validated time, rate limit remaining (cached 60s server-side)
   GET  /healthz              → JSON health check (no auth required); pings DB, returns 503 on failure
```

## Phases

### Phase 1: Scaffolding and Docker

- Initialize Go module (path must match actual GitHub repo — see Tech Stack note)
- `go.mod` with Go 1.26 pin
- `main.go` skeleton: config load, HTTP server with timeouts (`ReadTimeout: 15s`, `WriteTimeout: 30s`, `IdleTimeout: 60s`, `ReadHeaderTimeout: 5s`), graceful shutdown (SIGTERM/SIGINT)
- `internal/config/config.go`: load from env vars (see Env Var Table). Reads `GHVAULT_ENCRYPTION_KEY` from env or from file `/run/secrets/encryption_key` (Docker secret). Both sources are base64-decoded to raw 32 bytes.
- `Dockerfile` (multi-stage):
  ```dockerfile
  FROM golang:1.26-alpine AS build
  WORKDIR /src && COPY go.mod go.sum ./ && RUN go mod download && COPY . .
  RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /gh-vault .
  FROM alpine:3.20
  RUN apk add --no-cache git ca-certificates
  COPY --from=build /gh-vault /usr/local/bin/gh-vault
  ENTRYPOINT ["gh-vault"]
  ```
- `docker-compose.yml` with volumes, secrets, healthcheck directive (see Docker Compose section)
- `.env.example`: `GHVAULT_ENCRYPTION_KEY=<run: openssl rand -base64 32 > ./encryption_key && chmod 600 ./encryption_key>`
- `internal/model/model.go`: all domain types (see Domain Model Types)

### Phase 2: Store Layer

- `internal/store/db.go`: open SQLite with DSN `file:<path>?_pragma=foreign_keys(1)&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)`, run migrations via `PRAGMA user_version`
- `internal/store/repos.go`: implements `RepoStore` interface
- `internal/store/logs.go`: implements `LogStore` interface
- `internal/store/sessions.go`: implements `SessionStore` interface (create, get, delete, delete-expired)
- `internal/store/settings.go`: implements `SettingsStore` interface
- `internal/store/users.go`: implements `UserStore` interface
- `internal/store/migrations.go`: schema from Data Model section, migration runner (read `PRAGMA user_version`, iterate ordered migrations, apply each in transaction, bump version after each)
- Tests: `_test.go` per file using `t.TempDir()` for throwaway SQLite databases

### Phase 3: GitHub Client

- `internal/github/client.go`: implements `Client`. Constructor: `NewClient(tokenProvider)`. Queries token on every API call.
  - `ListOwnedRepos`: paginate with `ListByAuthenticatedUser`, filter `fork==false`
  - `GetRepo`, `ArchiveRepo`, `DeleteRepo`: standard go-github calls
  - `RateLimitStatus`: call `client.RateLimits(ctx)`, return `Remaining`, `Limit`, `Reset`
- `internal/github/dbtokenprovider.go`: implements `TokenProvider`. AES-256-GCM, 12-byte nonce, reads/writes `github_token` in `secrets`. In-memory cache with `sync.Mutex`.
- Rate limit handling: check cached limit before each call. If `Remaining == 0`, sleep until `Reset` or return error.
- Tests: `httptest.NewServer` with fixture JSON

### Phase 4: Sync Engine

- `internal/sync/syncer.go`: implements `Syncer` interface
- `SyncRepos` diff logic:
  1. Fetch all owned repos from GitHub via `Client.ListOwnedRepos`
  2. Fetch all repos from store via `RepoStore.List`
  3. **New**: in GitHub but not in store → `RepoStore.Upsert` with `Format: model.FormatClone` (required — CHECK constraint rejects empty)
  4. **Updated**: in both (matched by `github_id`), metadata differs → update metadata (size, language, last_push, github_url, `github_archived`)
  5. **Renamed**: same `github_id` but `name` changed → `SetOwnerName` + `SetBackupPath` with new path after `os.Rename` on disk
  6. **Transferred**: same `github_id` but `owner` changed → same as rename
  7. **Deleted**: in store but not in GitHub → `os.Stat(repo.BackupPath)`: if ENOENT → no backup, mark deleted; if exists AND `verified_at` set → mark `github_deleted=TRUE`; if exists but not verified → log warning, skip
  8. **Unchanged**: in both, no metadata changes → increment `Unchanged` counter
- Returns `SyncResult` with counts and error totals (each error logged via `slog.Error`)
- **Dry-run**: sync is read-only against GitHub (no API mutations). Local filesystem operations (path renames) still execute. See Phase 8 for canonical dry-run definition.
- After sync completes, run auto-archive check (see Phase 6 auto-archive job) for repos matching criteria.
- Tests: mock `Client` + in-memory `RepoStore`, verify each diff case

### Phase 5: Backup Engine

- `internal/backup/engine.go`: implements `Engine` interface. Constructor: `NewEngine(backupDir string, tokenProvider TokenProvider, repos store.RepoStore) *Engine`
- Git auth: URLs constructed as `https://x-access-token:<token>@github.com/owner/name.git`. Token from `tokenProvider.GetToken(ctx)`, never logged (redact in any log output). **Leak surface**: token appears in `/proc/<pid>/cmdline` briefly during exec. Mitigation: use `git -c credential.helper=<script>` with a helper script that feeds the token via stdin if this is unacceptable.
- Operations:
  - `CloneRepo`: `git clone --bare <authed-url> /backups/active/owner/name.git`. If dir exists, runs `fetch --prune` instead (idempotent — scheduler calls this for both fresh and existing repos).
  - `FetchRepo`: `git --git-dir=/backups/active/owner/name.git fetch --prune origin`
  - `CreateBundle`: requires bare clone at expected path. If missing, return error. `git bundle create /backups/archived/owner/name.bundle --all` from the bare clone dir.
  - `SwitchToBundle`: (1) create bundle to temp path, (2) `git bundle verify` temp, (3) rename temp → final path, (4) remove bare clone dir, (5) on success call `RepoStore.SetFormat(repo.ID, FormatBundle, newBackupPath)` and set `verified_at = nil`. Atomic via temp-dir-then-rename. Deferred `os.RemoveAll(tempPath)` on failure.
  - `SwitchToClone`: (1) if dest dir exists, verify it's a bare git repo of the same repo (`git -C dest remote get-url origin` matches), then remove it, (2) `git clone --bare /backups/archived/owner/name.bundle /backups/active/owner/name.git`, (3) `git -C ... remote set-url origin <authed-url>`, (4) `git -C ... fetch origin --prune`, (5) `git fsck`, (6) remove bundle file, (7) on success call `RepoStore.SetFormat(repo.ID, FormatClone, newBackupPath)` and set `verified_at = nil`. Deferred `os.RemoveAll(tempClonePath)` on failure.
  - `Verify`: for clones → `git --git-dir=<path> fsck --no-dangling`. For bundles → `git bundle verify <path>`. On success, update `verified_at` in store.
- Tests: use `t.TempDir()` as backup dir, create real bare clones of small test repos

### Phase 6: Scheduler

- `internal/scheduler/scheduler.go`: implements `Scheduler` interface
- Uses `robfig/cron/v3` with standard 5-field cron (no seconds)
- Only `cron_schedule` setting is user-editable (controls sync job). Other jobs run on hardcoded schedules.
- Jobs:
  - `sync`: calls `Syncer.SyncRepos` — schedule from `cron_schedule` setting (default `0 3 1 * *`)
  - `backup`: calls backup for all active repos (fan-out with `errgroup`, max 3 concurrent) — daily at 02:00
  - `auto_archive`: calls `workflow.ArchiveEligible()` — daily at 03:00 (checks `auto_archive` flag + `auto_archive_days` + `LastPush`)
  - `verify`: calls `Engine.Verify` for all repos — weekly Sunday 04:00
  - `log_cleanup`: deletes logs older than `log_retention_days` setting — daily at 05:00
  - `session_cleanup`: calls `SessionStore.DeleteExpired` — daily at 05:30
- **Job dedup**: `sync.Map[string]bool` tracks running jobs. Before execution, `LoadOrStore`. If already running, skip with log message. Delete on completion.
- **Cron hot-reload**: `ReloadCron(expr)` stops the cron scheduler, removes all entries, adds new entry with updated expression, restarts. Called from `POST /settings`.
- `NextRun(jobName)` returns next scheduled time for a specific job (dashboard shows next sync run by default)
- Graceful shutdown: `Stop()` waits for running jobs to finish (with 30s timeout)
- Tests: verify dedup (start job, try to start again, confirm skip), verify reload

### Phase 7: Web Dashboard

- `internal/web/server.go`: chi router + middleware stack:
  1. State machine middleware (see Routing section)
  2. Session middleware (read cookie, load session, inject user into context)
  3. CSRF middleware (validate token on all POST, skip for /login, /setup, /healthz)
  4. Logging middleware (slog, structured)
  5. Security headers middleware: `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Content-Security-Policy: default-src 'self'`, `Referrer-Policy: strict-origin-when-cross-origin`
  6. Static file serving (`embed.FS` from `internal/web/static/`)
- HTTP server timeouts: `ReadTimeout: 15s`, `WriteTimeout: 30s`, `IdleTimeout: 60s`, `ReadHeaderTimeout: 5s`
- `internal/web/handlers.go`: all route handlers
- `internal/web/middleware.go`: auth, CSRF, state machine, logging
- `internal/web/csrf.go`: token generation (crypto/rand 32 bytes, hex) and validation
- `internal/web/templates/`: Go HTML templates with htmx — `layout.html` (base + Tailwind + htmx), `dashboard.html` (stats), `repos.html` (table + actions), `logs.html` (activity), `settings.html` (cron, dry-run, token), `setup.html` (wizard), `login.html`
- `internal/web/static/`: vendored `tailwind.css`, `htmx.min.js`, `app.js` (minimal helpers)
- `POST /settings/token`: takes `token` form field, validates via GitHub API, encrypts via `TokenProvider.SetToken`, stores in `secrets`, updates cache. Redirect to `/`.
- Delete endpoint: `POST /repos/{id}/delete` requires `name` field. Server compares to `repo.Name`. Mismatch → 400.
- Backup endpoint: `POST /repos/{id}/backup` is async. Returns 202. Runs in goroutine, logs results.
- Archive endpoint: `POST /repos/{id}/archive` — triggers GitHub archive (PATCH archived:true). Requires backup to exist and be verified. Calls `github.Client.ArchiveRepo`.
- Tests: `httptest.NewServer` with test store, verify routing, auth, CSRF

### Phase 8: Archive and Delete Workflow

- `internal/workflow/archive.go`:
  ```go
  // ArchiveWorkflow interface
  type ArchiveWorkflow interface {
      RunEligible(ctx context.Context) error  // archives repos where auto_archive=true AND last_push < N days AND verified
  }
  // Constructor: NewArchive(repos store.RepoStore, logs store.LogStore, settings store.SettingsStore, client github.Client) *ArchiveWorkflow
  ```
  1. Verify backup exists and `verified_at` is set (safety: never delete unverified)
  2. If `auto_archive` enabled AND `auto_archive_days > 0` AND `repo.LastPush != nil` AND `time.Since(*repo.LastPush) > time.Duration(auto_archive_days)*24*time.Hour`: call `Client.ArchiveRepo`
  3. Manual delete: server checks posted repo name matches (see Phase 7). Then calls `Client.DeleteRepo`. Then marks `github_deleted=TRUE` in store.
  4. All actions logged to `logs` table
- **Dry-run semantics** (canonical definition — when `dry_run` setting is `true`, only GitHub API mutations are blocked):
  - Sync: runs normally (read-only against GitHub)
  - Backup: runs normally (local operation)
  - Archive: logs "would archive owner/name" but does NOT call GitHub API
  - Delete: logs "would delete owner/name" but does NOT call GitHub API
  - Format switch: runs normally (local operation)
- Tests: mock `Client`, verify API calls are skipped in dry-run

## File Structure

```
gh-vault/
├── main.go, go.mod, go.sum, Dockerfile, docker-compose.yml, .env.example
├── internal/
│   ├── model/model.go
│   ├── config/config.go
│   ├── store/
│   │   ├── db.go, migrations.go, repos.go, logs.go, sessions.go, settings.go, users.go
│   │   └── *_test.go (one per source file)
│   ├── github/
│   │   ├── client.go, dbtokenprovider.go, client_test.go, dbtokenprovider_test.go
│   ├── sync/syncer.go, syncer_test.go
│   ├── backup/engine.go, engine_test.go
│   ├── scheduler/scheduler.go, scheduler_test.go
│   ├── workflow/archive.go, archive_test.go
│   └── web/
│       ├── server.go, handlers.go, middleware.go, csrf.go
│       ├── handlers_test.go, middleware_test.go, server_test.go
│       ├── templates/ (layout, dashboard, repos, logs, settings, setup, login)
│       └── static/ (tailwind.css, htmx.min.js, app.js)
└── README.md
```

## Environment Variables

| Variable                 | Required | Default                 | Description                                                                                                                                                                                                       |
| ------------------------ | -------- | ----------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `GHVAULT_ENCRYPTION_KEY` | No       | —                       | 32 bytes, base64. AES-256 key for encrypting the GitHub token in SQLite. If not provided, auto-generated on first run and saved to `{DATA_DIR}/encryption_key`. Can also be set via Docker secret (`/run/secrets/encryption_key`). Losing the key = re-enter token. |
| `GHVAULT_PORT`           | No       | `8090`                  | Port the web dashboard listens on                                                                                                                                                                                 |
| `GHVAULT_BACKUP_DIR`     | No       | `/backups`              | Container path where git clones and bundles are stored. Set in docker-compose volume mapping.                                                                                                                     |
| `GHVAULT_DATA_DIR`       | No       | `/config`               | Container path where SQLite DB and encrypted secrets are stored. Set in docker-compose volume mapping.                                                                                                            |
| `GHVAULT_BASE_URL`       | No       | `http://localhost:8090` | Public-facing URL. If `https://`, session cookies get `Secure` flag.                                                                                                                                              |

No `GITHUB_TOKEN` env var. The GitHub token is set via the web UI (`POST /settings/token`) and stored encrypted in SQLite.

**Token rotation**: `POST /settings/token` with the new token. Takes effect immediately — no container restart needed.

## Testing Strategy

| Package     | Pattern                                        | Scope                                                                       |
| ----------- | ---------------------------------------------- | --------------------------------------------------------------------------- |
| `store`     | `t.TempDir()` for throwaway SQLite DBs         | CRUD, migrations, constraints, indexes                                      |
| `github`    | `httptest.NewServer` with fixture JSON         | API calls, pagination, rate limit handling, token encrypt/decrypt           |
| `sync`      | Mock `Client` + real `store`                   | Diff logic: new, updated, renamed, transferred, deleted, unchanged          |
| `backup`    | `t.TempDir()` as backup dir, real git commands | Clone, fetch, bundle, verify, switch. `TestMain` skips if `git` not in PATH |
| `scheduler` | Mock dependencies, short cron intervals        | Dedup, reload, graceful stop                                                |
| `workflow`  | Mock `Client` + real `store`                   | Dry-run skips API, safety checks, nil-guard on LastPush                     |
| `web`       | `httptest.NewServer` + test `store`            | Routing, auth redirects, CSRF, state machine                                |

Run all: `go test ./...`

## Docker Compose (TrueNAS)

```yaml
services:
  gh-vault:
    build: .
    container_name: gh-vault
    restart: unless-stopped
    ports:
      - "8090:8090"
    volumes:
      - /mnt/pool/gh-vault/config:/config # TODO: your ZFS dataset for app state
      - /mnt/pool/gh-vault/backups:/backups # TODO: your ZFS dataset for repo backups
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
    file: ./encryption_key  # Generate: openssl rand -base64 32 > ./encryption_key && chmod 600 ./encryption_key
```

`GHVAULT_ENCRYPTION_KEY` is mounted as a Docker secret (file at `/run/secrets/encryption_key`), not a plaintext env var. The config loader reads from env or falls back to this file path.

## Volume Layout & Restore Procedure

Two separate host paths are bind-mounted into the container. On TrueNAS, create one ZFS dataset per path.

| Container path | Host path (example)          | Contents                                          |
| -------------- | ---------------------------- | ------------------------------------------------- |
| `/config`      | `/mnt/pool/gh-vault/config`  | SQLite DB, encrypted secrets, app state           |
| `/backups`     | `/mnt/pool/gh-vault/backups` | Git clones (`active/`), git bundles (`archived/`) |

**Why separate?** `/config` is small and churns frequently (DB writes). `/backups` is large and append-mostly. Separate datasets allow independent snapshots and retention policies.

**Restore procedure:**

1. Stop the container: `docker compose down`
2. Restore `/config` dataset (contains DB + encrypted secrets)
3. Ensure the encryption key file is in place (`./encryption_key`)
4. Start the container: `docker compose up -d`
5. Optionally restore `/backups` dataset (repo clones and bundles)
6. Run `POST /trigger/sync` to reconcile — syncer detects missing backup paths and re-clones from GitHub

## Key Design Decisions

1. **`git` CLI via exec, not `go-git`**: More reliable, handles edge cases, simpler. Docker image includes `git` from Alpine.
2. **SQLite over Postgres**: Zero setup, single file, perfect for this scale. WAL mode handles concurrent reads from web UI + writes from backup.
3. **htmx over React**: No build step, no npm, no JS framework overhead. Perfect for an admin dashboard.
4. **Session auth with bcrypt**: Single user, self-hosted. First-run wizard sets credentials. No env var auth.
5. **Encrypted token in SQLite**: GitHub token stored AES-256-GCM encrypted. Encryption key is the only auth env var. Token rotatable via web UI without restart. Losing the encryption key means re-entering the token.
6. **Bare clones for active repos, bundles for archived**: Space-efficient clones support incremental fetch; bundles are single-file archives for offline storage.
7. **Sync package separate from GitHub package**: GitHub client is a pure API wrapper. Sync logic (diff, rename, transfer detection) lives in its own package.
