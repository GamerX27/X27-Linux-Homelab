// Package config reads and writes /etc/dashboard.conf, a root-only KEY=value file in the
// same style as /etc/autoupdate.conf. It's parsed, never sourced. The pairing password and
// the node's API token are stored only as salted SHA-256 hashes: both are long random
// values, so a slow hash adds nothing.
package config

import (
	"bufio"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

const (
	ModeMain = "main"
	ModeNode = "node"

	DefaultPort = 9090

	// Compose presets offered when creating a stack: a Forgejo/Gitea repository with one
	// folder per service under Composes/.
	DefaultPresetsRepo = "https://codeberg.org/X27/Docker-X27-Composes"
)

// Path and StateDir can be moved for development with DASHBOARD_CONF / DASHBOARD_STATE.
var (
	Path     = envOr("DASHBOARD_CONF", "/etc/dashboard.conf")
	StateDir = envOr("DASHBOARD_STATE", "/var/lib/dashboard")
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

type Config struct {
	Mode      string // main or node
	Port      int
	PairHash  string // node: hash of the pending pairing password, empty when none
	TokenHash string // node: hash of the API token the paired main node uses
	Presets   string // main: repository the compose presets come from
}

func Default() Config {
	return Config{Mode: ModeMain, Port: DefaultPort, Presets: DefaultPresetsRepo}
}

// Load returns the defaults when the file doesn't exist yet.
func Load() (Config, error) {
	c := Default()
	f, err := os.Open(Path)
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch k {
		case "MODE":
			if v == ModeMain || v == ModeNode {
				c.Mode = v
			}
		case "PORT":
			if p, err := strconv.Atoi(v); err == nil && ValidPort(p) {
				c.Port = p
			}
		case "PAIR_HASH":
			c.PairHash = v
		case "TOKEN_HASH":
			c.TokenHash = v
		case "PRESETS_REPO":
			if v != "" {
				c.Presets = v
			}
		}
	}
	return c, sc.Err()
}

func ValidPort(p int) bool { return p >= 1 && p <= 65535 }

// Save writes atomically with mode 600.
func (c Config) Save() error {
	body := fmt.Sprintf("# Written by dashboard. Change it with: dashboard enable ... / dashboard port ... / dashboard pair\n"+
		"MODE=%s\nPORT=%d\nPAIR_HASH=%s\nTOKEN_HASH=%s\nPRESETS_REPO=%s\n", c.Mode, c.Port, c.PairHash, c.TokenHash, c.Presets)
	tmp, err := os.CreateTemp(filepath.Dir(Path), ".dashboard.conf.")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(body); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), Path)
}

// Hash returns "salt:sha256(salt+secret)" in hex.
func Hash(secret string) string {
	salt := make([]byte, 16)
	rand.Read(salt)
	sum := sha256.Sum256(append(salt, secret...))
	return hex.EncodeToString(salt) + ":" + hex.EncodeToString(sum[:])
}

// Check compares in constant time. An empty hash never matches.
func Check(hash, secret string) bool {
	saltHex, sumHex, ok := strings.Cut(hash, ":")
	if !ok {
		return false
	}
	salt, err1 := hex.DecodeString(saltHex)
	want, err2 := hex.DecodeString(sumHex)
	if err1 != nil || err2 != nil || len(salt) == 0 {
		return false
	}
	got := sha256.Sum256(append(salt, secret...))
	return subtle.ConstantTimeCompare(got[:], want) == 1
}

// No 0/O, 1/I/L: it's typed by hand from one screen into another.
const pairAlphabet = "23456789ABCDEFGHJKMNPQRSTUVWXYZ"

// NewPairPassword returns e.g. "K7QM-2XRT-9HVA-PWE4-CN3S" (about 99 bits).
func NewPairPassword() string {
	b := make([]byte, 20)
	rand.Read(b)
	var sb strings.Builder
	for i, v := range b {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteByte(pairAlphabet[int(v)%len(pairAlphabet)])
	}
	return sb.String()
}

// NormalizePairPassword accepts lower case, spaces and missing dashes.
func NormalizePairPassword(s string) string {
	var sb strings.Builder
	for _, r := range strings.ToUpper(s) {
		if strings.ContainsRune(pairAlphabet, r) {
			sb.WriteRune(r)
		}
	}
	raw := sb.String()
	sb.Reset()
	for i, r := range raw {
		if i > 0 && i%4 == 0 {
			sb.WriteByte('-')
		}
		sb.WriteRune(r)
	}
	return sb.String()
}

// NewToken returns 256 random bits in hex.
func NewToken() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}
