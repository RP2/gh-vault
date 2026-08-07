// Package github contains GitHub-specific functionality for gh-vault,
// including secure storage and retrieval of the personal access token used
// to authenticate against the GitHub API.
package github

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"

	"github.com/RP2/gh-vault/internal/store"
)

const (
	tokenKey     = "github_token"
	gcmNonceSize = 12
)

// TokenProvider defines how the GitHub API token is read and written.
// Implementations are responsible for encrypting the token at rest and
// may cache the decrypted value to avoid repeated decryption work.
type TokenProvider interface {
	GetToken(ctx context.Context) (string, error)
	SetToken(ctx context.Context, token string) error
}

// DBTokenProvider implements TokenProvider backed by a SecretStore.
//
// Encryption scheme:
//
//	The token is encrypted with AES-256-GCM and stored at the
//	"github_token" key. Every SetToken call generates a fresh
//	12-byte random nonce, so the same plaintext encrypted twice
//	produces unrelated ciphertexts. The 32-byte AES-256 key is
//	supplied by the caller (typically the base64-decoded value of
//	the ENCRYPTION_KEY environment variable).
//
//	cached  = plaintext
//	stored  = AES-256-GCM-Seal(encKey, nonce, plaintext, nil)
//	plaintext, err = AES-256-GCM-Open(encKey, nonce, stored, nil)
//
// The decrypted value is cached in-process under a mutex; subsequent
// GetToken calls return the cached string without touching the store,
// and SetToken overwrites the cache so reads see the latest write.
type DBTokenProvider struct {
	mu          sync.Mutex
	cache       string
	cacheValid  bool
	secretStore store.SecretStore
	encKey      []byte // 32-byte AES-256 key
	fingerprint string // hex-encoded SHA-256 prefix of encKey, for diagnostics
}

// NewDBTokenProvider returns a DBTokenProvider that reads and writes the
// GitHub token through secretStore, using encKey as the AES-256 key.
//
// encKey must be exactly 32 bytes; otherwise the constructor panics,
// because a wrong-sized key is a programming error rather than a
// runtime condition. A short hex fingerprint of encKey is precomputed
// and included in error messages to help correlate failures with key
// rotations without leaking the key itself.
func NewDBTokenProvider(secretStore store.SecretStore, encKey []byte) *DBTokenProvider {
	if len(encKey) != 32 {
		panic(fmt.Sprintf("github: NewDBTokenProvider requires a 32-byte encryption key, got %d bytes", len(encKey)))
	}
	sum := sha256.Sum256(encKey)
	return &DBTokenProvider{
		secretStore: secretStore,
		encKey:      encKey,
		fingerprint: hex.EncodeToString(sum[:8]),
	}
}

// GetToken returns the decrypted GitHub token. If no token has been
// stored yet, it returns ("", nil) so callers can distinguish "not
// configured" from "decryption failed". The first successful read
// populates the in-memory cache; later SetToken calls overwrite it.
func (p *DBTokenProvider) GetToken(_ context.Context) (string, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.cacheValid {
		return p.cache, nil
	}

	value, nonce, err := p.secretStore.Get(tokenKey)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			p.cache = ""
			p.cacheValid = true
			return "", nil
		}
		return "", fmt.Errorf("github: read token from store: %w", err)
	}
	if len(value) == 0 {
		p.cache = ""
		p.cacheValid = true
		return "", nil
	}

	plaintext, err := p.decrypt(value, nonce)
	if err != nil {
		return "", err
	}

	p.cache = string(plaintext)
	p.cacheValid = true
	return p.cache, nil
}

// SetToken encrypts token with AES-256-GCM using a freshly generated
// 12-byte nonce and writes the ciphertext to the secret store. The
// in-memory cache is updated so subsequent GetToken calls return the
// new value without re-reading from the store.
func (p *DBTokenProvider) SetToken(_ context.Context, token string) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	block, err := aes.NewCipher(p.encKey)
	if err != nil {
		return fmt.Errorf("github: init aes cipher (fingerprint %s): %w", p.fingerprint, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return fmt.Errorf("github: init gcm (fingerprint %s): %w", p.fingerprint, err)
	}

	nonce := make([]byte, gcmNonceSize)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("github: generate nonce: %w", err)
	}

	ciphertext := gcm.Seal(nil, nonce, []byte(token), nil)
	if err := p.secretStore.Set(tokenKey, ciphertext, nonce); err != nil {
		return fmt.Errorf("github: write token to store: %w", err)
	}

	p.cache = token
	p.cacheValid = true
	return nil
}

// decrypt opens the AES-256-GCM ciphertext under the provider's key.
// It centralises the cipher construction so GetToken stays linear and
// the wrapped error path stays consistent.
func (p *DBTokenProvider) decrypt(value, nonce []byte) ([]byte, error) {
	block, err := aes.NewCipher(p.encKey)
	if err != nil {
		return nil, fmt.Errorf("github: init aes cipher (fingerprint %s): %w", p.fingerprint, err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("github: init gcm (fingerprint %s): %w", p.fingerprint, err)
	}
	plaintext, err := gcm.Open(nil, nonce, value, nil)
	if err != nil {
		return nil, fmt.Errorf("github: decrypt token (fingerprint %s): %w", p.fingerprint, err)
	}
	return plaintext, nil
}
