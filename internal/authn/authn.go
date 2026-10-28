// Package authn is accounts for the console: roles, password hashing and session tokens.
//
// Passwords use PBKDF2-HMAC-SHA256 from the standard library with a per-user salt, the
// iteration count stored with the hash so it can be raised later. Session tokens are 32
// random bytes; only their SHA-256 is stored, so reading the database does not yield a
// usable session.
package authn

import (
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
)

// Role is what a person may do. See docs/design/006-console.md.
type Role string

const (
	Viewer    Role = "viewer"
	Developer Role = "developer"
	Admin     Role = "admin"
)

// Valid reports whether r is a known role.
func (r Role) Valid() bool { return r == Viewer || r == Developer || r == Admin }

// Rank orders roles so a check can ask for "at least developer".
func (r Role) Rank() int {
	switch r {
	case Viewer:
		return 1
	case Developer:
		return 2
	case Admin:
		return 3
	}
	return 0
}

// AtLeast reports whether r grants everything min does.
func (r Role) AtLeast(min Role) bool { return r.Rank() >= min.Rank() && min.Rank() > 0 }

// Iterations is the PBKDF2 work factor for new hashes (OWASP guidance for SHA-256). It is a
// variable only so tests can lower it; nothing else changes it.
var Iterations = defaultIterations

const defaultIterations = 600_000

const (
	// MinPasswordLength is enforced when a password is set.
	MinPasswordLength = 12
	// MaxPasswordLength bounds the work an attacker can force with a huge password.
	MaxPasswordLength = 256
	saltBytes         = 16
	hashBytes         = 32
)

// ErrWeakPassword says why a password was refused.
type ErrWeakPassword struct{ Reason string }

func (e ErrWeakPassword) Error() string { return e.Reason }

// CheckPassword enforces the password policy: length, and not just the username.
func CheckPassword(username, password string) error {
	switch {
	case len(password) < MinPasswordLength:
		return ErrWeakPassword{fmt.Sprintf("password must be at least %d characters", MinPasswordLength)}
	case len(password) > MaxPasswordLength:
		return ErrWeakPassword{fmt.Sprintf("password must be at most %d characters", MaxPasswordLength)}
	case strings.EqualFold(password, username):
		return ErrWeakPassword{"password must not be the username"}
	case strings.Count(password, string(password[0])) == len(password):
		return ErrWeakPassword{"password must not be one repeated character"}
	}
	return nil
}

// HashPassword returns "pbkdf2-sha256$<iterations>$<salt>$<hash>".
func HashPassword(password string) (string, error) {
	salt := make([]byte, saltBytes)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	return hashWith(password, salt, Iterations)
}

func hashWith(password string, salt []byte, iter int) (string, error) {
	dk, err := pbkdf2.Key(sha256.New, password, salt, iter, hashBytes)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s", iter,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(dk)), nil
}

// VerifyPassword checks password against a stored hash in constant time. needsRehash is true
// when the hash uses fewer iterations than the current setting.
func VerifyPassword(password, stored string) (ok, needsRehash bool) {
	parts := strings.Split(stored, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false, false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter < 1 || iter > 10_000_000 {
		return false, false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false, false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false, false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false, false
	}
	if subtle.ConstantTimeCompare(got, want) != 1 {
		return false, false
	}
	return true, iter < Iterations
}

// DummyVerify burns about as much time as a real check, so a login attempt for a user that
// does not exist cannot be told apart by timing.
func DummyVerify(password string) {
	dummyOnce.Do(func() {
		dummyHash, _ = hashWith("dummy-password-for-timing", make([]byte, saltBytes), Iterations)
	})
	VerifyPassword(password, dummyHash)
}

var (
	dummyOnce sync.Once
	dummyHash string
)

// NewToken returns a random session token to give to the browser, and the id to store.
func NewToken() (token, id string, err error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", "", err
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, TokenID(token), nil
}

// TokenID is the stored identifier of a token: its SHA-256 in hex.
func TokenID(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// NewID returns a random identifier for a user or other record.
func NewID() string {
	b := make([]byte, 9)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// ValidUsername reports whether name is acceptable: 3 to 40 of letters, digits and . _ - @.
func ValidUsername(name string) error {
	if len(name) < 3 || len(name) > 40 {
		return errors.New("username must be 3 to 40 characters")
	}
	for _, r := range name {
		ok := r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' || r == '@'
		if !ok {
			return errors.New("username may contain only letters, digits and . _ - @")
		}
	}
	return nil
}
