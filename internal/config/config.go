package config

import (
	"crypto/rand"
	"encoding/base64"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
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
	// BaseURL is the public-facing URL used for redirect targets.
	BaseURL string
	// EncryptionKey is the raw 32-byte AES-256 key used to encrypt the
	// GitHub token. Decoded from base64.
	EncryptionKey []byte
	// DisableTLS, when true, serves plain HTTP instead of HTTPS.
	DisableTLS bool
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

	if v := os.Getenv("PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			slog.Error("invalid PORT", "value", v, "error", err)
			os.Exit(1)
		}
		if p < 1 || p > 65535 {
			slog.Error("invalid port", "port", p)
			os.Exit(1)
		}
		c.Port = p
	}

	if v := os.Getenv("BACKUP_DIR"); v != "" {
		c.BackupDir = v
	}

	if v := os.Getenv("DATA_DIR"); v != "" {
		c.DataDir = v
	}

	if v := os.Getenv("BASE_URL"); v != "" {
		c.BaseURL = v
	}

	if v := os.Getenv("DISABLE_TLS"); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			c.DisableTLS = b
		} else {
			switch strings.ToLower(v) {
			case "yes", "on", "y":
				c.DisableTLS = true
			case "no", "off", "n":
				c.DisableTLS = false
			}
		}
	}

	c.EncryptionKey = loadEncryptionKey(c.DataDir)

	return c
}

func loadEncryptionKey(dataDir string) []byte {
	raw := strings.TrimSpace(os.Getenv("ENCRYPTION_KEY"))

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
		keyPath := filepath.Join(dataDir, "encryption_key")
		if data, err := os.ReadFile(keyPath); err == nil {
			raw = strings.TrimSpace(string(data))
			if raw != "" {
				slog.Info("loaded encryption key from file", "path", keyPath)
			}
		} else if !errors.Is(err, fs.ErrNotExist) {
			slog.Error("failed to read encryption key file", "path", keyPath, "error", err)
			os.Exit(1)
		}
	}

	if raw == "" {
		dbPath := filepath.Join(dataDir, "gh-vault.db")
		if _, err := os.Stat(dbPath); err == nil {
			slog.Error("ENCRYPTION_KEY is required when a database already exists. Set the ENCRYPTION_KEY environment variable to a base64-encoded 32-byte key. Auto-generation refused to prevent silent key rotation.")
			os.Exit(1)
		}

		keyBytes := make([]byte, 32)
		if _, err := rand.Read(keyBytes); err != nil {
			slog.Error("failed to generate encryption key", "error", err)
			os.Exit(1)
		}
		raw = base64.StdEncoding.EncodeToString(keyBytes)

		if err := os.MkdirAll(dataDir, 0700); err != nil {
			slog.Error("failed to create data directory", "path", dataDir, "error", err)
			os.Exit(1)
		}

		keyPath := filepath.Join(dataDir, "encryption_key")
		tmpPath := keyPath + ".tmp"
		if err := os.WriteFile(tmpPath, []byte(raw+"\n"), 0600); err != nil {
			slog.Error("failed to write encryption key", "path", tmpPath, "error", err)
			os.Remove(tmpPath)
			os.Exit(1)
		}
		if err := os.Chmod(tmpPath, 0600); err != nil {
			slog.Error("failed to chmod encryption key", "path", tmpPath, "error", err)
			os.Remove(tmpPath)
			os.Exit(1)
		}
		if err := os.Rename(tmpPath, keyPath); err != nil {
			slog.Error("failed to rename encryption key", "path", keyPath, "error", err)
			os.Remove(tmpPath)
			os.Exit(1)
		}
		slog.Info("auto-generated encryption key")
	}

	decoded, err := base64.StdEncoding.DecodeString(raw)
	if err != nil {
		slog.Error("failed to base64-decode ENCRYPTION_KEY", "error", err)
		os.Exit(1)
	}

	if len(decoded) != 32 {
		slog.Error("ENCRYPTION_KEY must be 32 bytes after decoding", "got", len(decoded))
		os.Exit(1)
	}

	return decoded
}
