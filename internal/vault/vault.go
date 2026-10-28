// Package vault encrypts secret values for storage. AES-256-GCM, a fresh random nonce for
// every seal, and the secret's name bound in as associated data so a stored value cannot be
// moved to a different name without failing to open.
//
// The key never leaves this process. Losing it loses every sealed value, which is the point:
// there is no recovery path an attacker could use either.
package vault

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// KeySize is the key length in bytes.
const KeySize = 32

// ErrCorrupt is returned when a sealed value does not open: wrong key, wrong name, or damage.
var ErrCorrupt = errors.New("vault: cannot open value (wrong key, wrong name, or damaged data)")

// Vault seals and opens values.
type Vault struct{ aead cipher.AEAD }

// New returns a Vault using key, which must be exactly KeySize bytes.
func New(key []byte) (*Vault, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("vault: key must be %d bytes, got %d", KeySize, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Vault{aead: aead}, nil
}

// ParseKey decodes a key given as standard or URL-safe base64.
func ParseKey(s string) ([]byte, error) {
	s = strings.TrimSpace(s)
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(s); err == nil && len(b) == KeySize {
			return b, nil
		}
	}
	return nil, fmt.Errorf("vault: the master key must be %d random bytes as base64 (generate one with: head -c 32 /dev/urandom | base64)", KeySize)
}

// LoadOrCreateKey returns the key from keyEnv if it is set, else from the file at path,
// creating the file with a new random key (mode 0600) if it does not exist. created is true
// when a new key was written; the caller should warn, because a key stored next to the data
// it protects only guards against casual reading of the database.
func LoadOrCreateKey(keyEnv, path string) (key []byte, created bool, err error) {
	if keyEnv != "" {
		key, err = ParseKey(keyEnv)
		return key, false, err
	}
	if b, err := os.ReadFile(path); err == nil {
		key, err := ParseKey(string(b))
		if err != nil {
			return nil, false, fmt.Errorf("%s: %w", path, err)
		}
		return key, false, nil
	} else if !os.IsNotExist(err) {
		return nil, false, err
	}
	key = make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, false, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, false, err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, false, err
	}
	_, werr := f.WriteString(base64.StdEncoding.EncodeToString(key) + "\n")
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		os.Remove(path)
		return nil, false, werr
	}
	return key, true, nil
}

// Seal encrypts value for the secret called name. The result is base64 text.
func (v *Vault) Seal(name, value string) (string, error) {
	nonce := make([]byte, v.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := v.aead.Seal(nonce, nonce, []byte(value), []byte(name))
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// Open decrypts a value produced by Seal for the same name.
func (v *Vault) Open(name, sealed string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(sealed)
	if err != nil || len(raw) < v.aead.NonceSize() {
		return "", ErrCorrupt
	}
	nonce, ct := raw[:v.aead.NonceSize()], raw[v.aead.NonceSize():]
	pt, err := v.aead.Open(nil, nonce, ct, []byte(name))
	if err != nil {
		return "", ErrCorrupt
	}
	return string(pt), nil
}
