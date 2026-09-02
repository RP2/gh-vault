package config

// The following code paths in config.go call os.Exit(1) and cannot be
// tested without killing the test binary:
// - Invalid PORT value (non-numeric, out of range)
// - Invalid base64 in ENCRYPTION_KEY
// - ENCRYPTION_KEY decoded length != 32
// - os.MkdirAll, os.WriteFile, os.Chmod, os.Rename failures
// - Unreadable Docker secret file (/run/secrets/encryption_key)
// - Unreadable dataDir/encryption_key file
// These are tested by inspection, not by automated tests.

import (
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func generateValidKey(t *testing.T) string {
	t.Helper()
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatalf("failed to generate random key: %v", err)
	}
	return base64.StdEncoding.EncodeToString(keyBytes)
}

func TestLoadDefaults(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", generateValidKey(t))
	t.Setenv("PORT", "")
	t.Setenv("BACKUP_DIR", "")
	t.Setenv("DATA_DIR", "")
	t.Setenv("BASE_URL", "")

	c := Load()

	if c.Port != 8090 {
		t.Errorf("Port = %d, want 8090", c.Port)
	}
	if c.BackupDir != "/backups" {
		t.Errorf("BackupDir = %q, want %q", c.BackupDir, "/backups")
	}
	if c.DataDir != "/config" {
		t.Errorf("DataDir = %q, want %q", c.DataDir, "/config")
	}
	if c.BaseURL != "http://localhost:8090" {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, "http://localhost:8090")
	}
}

func TestLoadPortOverride(t *testing.T) {
	t.Setenv("PORT", "9090")
	t.Setenv("ENCRYPTION_KEY", generateValidKey(t))

	c := Load()

	if c.Port != 9090 {
		t.Errorf("Port = %d, want 9090", c.Port)
	}
}

func TestLoadDirOverrides(t *testing.T) {
	t.Setenv("BACKUP_DIR", "/my/backups")
	t.Setenv("DATA_DIR", "/my/data")
	t.Setenv("ENCRYPTION_KEY", generateValidKey(t))

	c := Load()

	if c.BackupDir != "/my/backups" {
		t.Errorf("BackupDir = %q, want %q", c.BackupDir, "/my/backups")
	}
	if c.DataDir != "/my/data" {
		t.Errorf("DataDir = %q, want %q", c.DataDir, "/my/data")
	}
}

func TestLoadBaseURLOverride(t *testing.T) {
	t.Setenv("BASE_URL", "https://example.com")
	t.Setenv("ENCRYPTION_KEY", generateValidKey(t))

	c := Load()

	if c.BaseURL != "https://example.com" {
		t.Errorf("BaseURL = %q, want %q", c.BaseURL, "https://example.com")
	}
}

func TestLoadDisableTLS(t *testing.T) {
	t.Setenv("ENCRYPTION_KEY", generateValidKey(t))

	t.Run("default false", func(t *testing.T) {
		t.Setenv("DISABLE_TLS", "")

		c := Load()

		if c.DisableTLS {
			t.Errorf("DisableTLS = true, want false")
		}
	})

	trueValues := []string{"true", "TRUE", "True", "1", "yes", "on", "t", "T"}
	for _, v := range trueValues {
		t.Run(v+" is true", func(t *testing.T) {
			t.Setenv("DISABLE_TLS", v)

			c := Load()

			if !c.DisableTLS {
				t.Errorf("DisableTLS = false, want true for %q", v)
			}
		})
	}

	falseValues := []string{"false", "FALSE", "False", "0", "no", "off", "f", "F"}
	for _, v := range falseValues {
		t.Run(v+" is false", func(t *testing.T) {
			t.Setenv("DISABLE_TLS", v)

			c := Load()

			if c.DisableTLS {
				t.Errorf("DisableTLS = true, want false for %q", v)
			}
		})
	}
}

func TestEncryptionKeyFromEnv(t *testing.T) {
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatalf("failed to generate random key: %v", err)
	}
	encodedKey := base64.StdEncoding.EncodeToString(keyBytes)

	t.Run("clean", func(t *testing.T) {
		t.Setenv("ENCRYPTION_KEY", encodedKey)

		got := loadEncryptionKey(t.TempDir())

		if string(got) != string(keyBytes) {
			t.Errorf("key mismatch:\n  got:  %x\n  want: %x", got, keyBytes)
		}
	})

	t.Run("whitespace trimmed", func(t *testing.T) {
		t.Setenv("ENCRYPTION_KEY", "\n  "+encodedKey+"  \n")

		got := loadEncryptionKey(t.TempDir())

		if string(got) != string(keyBytes) {
			t.Errorf("key mismatch:\n  got:  %x\n  want: %x", got, keyBytes)
		}
	})
}

func TestEncryptionKeyAutoGenerate(t *testing.T) {
	if _, err := os.Stat("/run/secrets/encryption_key"); !os.IsNotExist(err) {
		t.Skip("test requires /run/secrets/encryption_key to be absent")
	}
	t.Setenv("ENCRYPTION_KEY", "")
	dataDir := t.TempDir()

	got := loadEncryptionKey(dataDir)

	if len(got) != 32 {
		t.Errorf("key length = %d, want 32", len(got))
	}

	keyPath := filepath.Join(dataDir, "encryption_key")
	if _, err := os.Stat(keyPath); err != nil {
		t.Errorf("expected key file to exist at %s: %v", keyPath, err)
	}

	content, err := os.ReadFile(keyPath)
	if err != nil {
		t.Fatalf("failed to read generated key file: %v", err)
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(content)))
	if err != nil {
		t.Fatalf("failed to decode generated key file content: %v", err)
	}
	if string(decoded) != string(got) {
		t.Errorf("file content does not match returned key:\n  file: %x\n  got:  %x", decoded, got)
	}

	fi, err := os.Stat(keyPath)
	if err != nil {
		t.Fatalf("failed to stat generated key file: %v", err)
	}
	if fi.Mode().Perm() != 0600 {
		t.Errorf("key file permissions = %o, want %o", fi.Mode().Perm(), 0600)
	}
}

func TestEncryptionKeyLoadFromFile(t *testing.T) {
	if _, err := os.Stat("/run/secrets/encryption_key"); !os.IsNotExist(err) {
		t.Skip("test requires /run/secrets/encryption_key to be absent")
	}
	keyBytes := make([]byte, 32)
	if _, err := rand.Read(keyBytes); err != nil {
		t.Fatalf("failed to generate random key: %v", err)
	}
	encodedKey := base64.StdEncoding.EncodeToString(keyBytes)

	dataDir := t.TempDir()
	keyPath := filepath.Join(dataDir, "encryption_key")
	if err := os.WriteFile(keyPath, []byte(encodedKey), 0600); err != nil {
		t.Fatalf("failed to write key file: %v", err)
	}

	t.Setenv("ENCRYPTION_KEY", "")

	got := loadEncryptionKey(dataDir)

	if string(got) != string(keyBytes) {
		t.Errorf("key mismatch:\n  got:  %x\n  want: %x", got, keyBytes)
	}
}
