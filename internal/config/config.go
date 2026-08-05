package config

import (
	"encoding/base64"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"strconv"
	"strings"
)

// Config holds the application configuration loaded from environment
// variables and Docker secret files.
type Config struct {
	// Port the web dashboard listens on.
	Port int
	// BackupDir is the container path where git clones and bundles are stored.
	BackupDir string
	// DataDir is the container path where the SQLite DB and encrypted secrets are stored.
	DataDir string
	// BaseURL is the public-facing URL. When it uses https, session cookies
	// receive the Secure flag.
	BaseURL string
	// EncryptionKey is the raw 32-byte AES-256 key used to encrypt the
	// GitHub token. Decoded from base64.
	EncryptionKey []byte
}

// Load reads configuration from environment variables and Docker secret
// files, falling back to defaults where applicable.
func Load() *Config {
	c := &Config{
		Port:      8090,
		BackupDir: "/backups",
		DataDir:   "/config",
		BaseURL:   "http://localhost:8090",
	}

	if v := os.Getenv("GHVAULT_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			slog.Error("invalid GHVAULT_PORT", "value", v, "error", err)
			os.Exit(1)
		}
		if p < 1 || p > 65535 {
			slog.Error("invalid port", "port", p)
			os.Exit(1)
		}
		c.Port = p
	}

	if v := os.Getenv("GHVAULT_BACKUP_DIR"); v != "" {
		c.BackupDir = v
	}

	if v := os.Getenv("GHVAULT_DATA_DIR"); v != "" {
		c.DataDir = v
	}

	if v := os.Getenv("GHVAULT_BASE_URL"); v != "" {
		c.BaseURL = v
	}

	c.EncryptionKey = loadEncryptionKey()

	return c
}

func loadEncryptionKey() []byte {
	raw := strings.TrimSpace(os.Getenv("GHVAULT_ENCRYPTION_KEY"))

	if raw == "" {
		data, err := os.ReadFile("/run/secrets/encryption_key")
		if err == nil {
			raw = strings.TrimSpace(string(data))
		} else if !errors.Is(err, fs.ErrNotExist) {
			slog.Error("failed to read /run/secrets/encryption_key", "error", err)
			os.Exit(1)
		}
	}

	if raw == "" {
		slog.Warn("GHVAULT_ENCRYPTION_KEY not set; encrypted operations unavailable until key is provided via web UI")
		return nil
	}

	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		slog.Error("failed to base64-decode GHVAULT_ENCRYPTION_KEY", "error", err)
		os.Exit(1)
	}

	if len(decoded) != 32 {
		slog.Error("GHVAULT_ENCRYPTION_KEY must be 32 bytes after decoding", "got", len(decoded))
		os.Exit(1)
	}

	return decoded
}
