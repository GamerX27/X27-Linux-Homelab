// Package apitokens holds the tokens AI clients use for the MCP server. A token belongs to
// the user who made it (tools act as them) and has a scope. Only a hash is stored, so a
// token is shown once, when it's made.
package apitokens

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/tools"
)

// Prefix makes the tokens recognizable (and findable by secret scanners).
const Prefix = "x27hl_"

type Token struct {
	ID        string      `json:"id"`
	Name      string      `json:"name"`
	Scope     tools.Scope `json:"scope"`
	Owner     string      `json:"owner"`
	CreatedAt int64       `json:"createdAt"`
	LastUsed  int64       `json:"lastUsed"`
	ExpiresAt int64       `json:"expiresAt,omitempty"` // 0 = never
	Hash      string      `json:"hash,omitempty"`
}

type Store struct {
	path   string
	mu     sync.Mutex
	tokens []Token
}

func Open(dir string) (*Store, error) {
	s := &Store{path: filepath.Join(dir, "tokens.json")}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	return s, json.Unmarshal(b, &s.tokens)
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.tokens, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

// List returns the tokens without their hashes.
func (s *Store) List() []Token {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []Token{}
	for _, t := range s.tokens {
		t.Hash = ""
		out = append(out, t)
	}
	return out
}

func randomHex(n int) string {
	b := make([]byte, n)
	rand.Read(b)
	return hex.EncodeToString(b)
}

// Create makes a token and returns it with its secret, the only time the secret is known.
func (s *Store) Create(owner, name string, scope tools.Scope, days int) (Token, string, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return Token{}, "", errors.New("give the token a name of 1-64 characters, e.g. the client using it")
	}
	if days < 0 || days > 3650 {
		return Token{}, "", errors.New("expiry must be 0 (never) to 3650 days")
	}
	id := randomHex(6)
	secret := Prefix + id + "_" + randomHex(24)
	t := Token{ID: id, Name: name, Scope: scope, Owner: owner, CreatedAt: time.Now().Unix(), Hash: config.Hash(secret)}
	if days > 0 {
		t.ExpiresAt = time.Now().AddDate(0, 0, days).Unix()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tokens = append(s.tokens, t)
	if err := s.saveLocked(); err != nil {
		return Token{}, "", err
	}
	t.Hash = ""
	return t, secret, nil
}

func (s *Store) Delete(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.tokens {
		if t.ID == id {
			s.tokens = append(s.tokens[:i], s.tokens[i+1:]...)
			return s.saveLocked()
		}
	}
	return errors.New("no such token")
}

// Verify finds the token for a secret. The id in the secret picks the entry, so only one
// hash is checked.
func (s *Store) Verify(secret string) (Token, error) {
	rest, ok := strings.CutPrefix(secret, Prefix)
	id, _, ok2 := strings.Cut(rest, "_")
	if !ok || !ok2 {
		return Token{}, errors.New("not a dashboard token")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, t := range s.tokens {
		if t.ID != id || !config.Check(t.Hash, secret) {
			continue
		}
		if t.ExpiresAt != 0 && time.Now().Unix() > t.ExpiresAt {
			return Token{}, errors.New("the token has expired")
		}
		// Written at most once a minute per token, not on every call.
		if now := time.Now().Unix(); now-t.LastUsed >= 60 {
			s.tokens[i].LastUsed = now
			s.saveLocked()
		}
		t.Hash = ""
		return t, nil
	}
	return Token{}, errors.New("unknown token")
}
