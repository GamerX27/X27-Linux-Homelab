package auth

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// Profiles keeps each user's Gravatar hash (SHA-256 of the trimmed, lower-cased email, as
// Gravatar expects) in /var/lib/dashboard/profiles.json. The email itself isn't stored.
type Profiles struct {
	path string
	mu   sync.Mutex
}

func NewProfiles(stateDir string) *Profiles {
	return &Profiles{path: filepath.Join(stateDir, "profiles.json")}
}

func GravatarHash(email string) string {
	sum := sha256.Sum256([]byte(strings.ToLower(strings.TrimSpace(email))))
	return hex.EncodeToString(sum[:])
}

func (p *Profiles) load() map[string]string {
	m := map[string]string{}
	if b, err := os.ReadFile(p.path); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

func (p *Profiles) Gravatar(user string) string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.load()[user]
}

// SetEmail stores the hash for an email, or clears it when email is empty.
func (p *Profiles) SetEmail(user, email string) (string, error) {
	email = strings.TrimSpace(email)
	hash := ""
	if email != "" {
		if a, err := mail.ParseAddress(email); err != nil || a.Address != email {
			return "", errors.New("that isn't an email address")
		}
		hash = GravatarHash(email)
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	m := p.load()
	if hash == "" {
		delete(m, user)
	} else {
		m[user] = hash
	}
	b, _ := json.MarshalIndent(m, "", "  ")
	if err := os.MkdirAll(filepath.Dir(p.path), 0o700); err != nil {
		return "", err
	}
	tmp := p.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return "", err
	}
	return hash, os.Rename(tmp, p.path)
}
