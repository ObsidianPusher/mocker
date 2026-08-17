package server

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"strings"
	"sync"
	"time"
)

type authManager struct {
	mu           sync.Mutex
	enabled      bool
	username     string
	passwordHash string
	sessions     map[string]time.Time
	sessionTTL   time.Duration
}

func newAuthManager(username, passwordHash string) *authManager {
	enabled := username != "" && passwordHash != ""
	return &authManager{
		enabled:      enabled,
		username:     username,
		passwordHash: passwordHash,
		sessions:     make(map[string]time.Time),
		sessionTTL:   24 * time.Hour,
	}
}

func (a *authManager) Enabled() bool {
	return a.enabled
}

func (a *authManager) Username() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.username
}

func (a *authManager) Verify(username, password string) bool {
	if !a.enabled {
		return true
	}
	a.mu.Lock()
	hash := a.passwordHash
	expectUser := a.username
	a.mu.Unlock()
	if username != expectUser {
		return false
	}
	return verifyPasswordHash(hash, password)
}

func (a *authManager) UpdateCredentials(username, passwordHash string) {
	a.mu.Lock()
	a.username = username
	a.passwordHash = passwordHash
	a.enabled = username != "" && passwordHash != ""
	a.sessions = make(map[string]time.Time)
	a.mu.Unlock()
}

func (a *authManager) CreateSession() (string, time.Time, error) {
	token, err := randomToken(32)
	if err != nil {
		return "", time.Time{}, err
	}
	expires := time.Now().Add(a.sessionTTL)
	a.mu.Lock()
	a.sessions[token] = expires
	a.mu.Unlock()
	return token, expires, nil
}

func (a *authManager) ValidateSession(token string) bool {
	if !a.enabled {
		return true
	}
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	expiry, ok := a.sessions[token]
	if !ok {
		return false
	}
	if time.Now().After(expiry) {
		delete(a.sessions, token)
		return false
	}
	return true
}

func (a *authManager) RevokeSession(token string) {
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
}

func randomToken(size int) (string, error) {
	buf := make([]byte, size)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(buf), nil
}

func hashPassword(password string) (string, error) {
	salt, err := randomToken(16)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256([]byte(salt + ":" + password))
	return "sha256$" + salt + "$" + hex.EncodeToString(sum[:]), nil
}

func verifyPasswordHash(hash, password string) bool {
	if hash == "" {
		return false
	}
	if strings.HasPrefix(hash, "sha256$") {
		parts := strings.Split(hash, "$")
		if len(parts) != 3 {
			return false
		}
		salt := parts[1]
		sum := sha256.Sum256([]byte(salt + ":" + password))
		expected, err := hex.DecodeString(parts[2])
		if err != nil {
			return false
		}
		return subtle.ConstantTimeCompare(sum[:], expected) == 1
	}
	return subtle.ConstantTimeCompare([]byte(hash), []byte(password)) == 1
}
