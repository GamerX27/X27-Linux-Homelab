package vip

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/nodes"
)

// A Bundle is what the main node copies to every other member of the virtual IP so it can
// stand in: the paired nodes (with their tokens), the API tokens, the main node's
// certificate (so browsers and MCP clients see the same one on the virtual IP whoever
// holds it), and the cluster settings without their secrets.
type Bundle struct {
	MainName string          `json:"mainName"`
	MainAddr string          `json:"mainAddr,omitempty"` // host:port where members reach the main node's cluster API
	Nodes    []nodes.Node    `json:"nodes"`
	Tokens   json.RawMessage `json:"tokens,omitempty"`
	Cluster  json.RawMessage `json:"cluster,omitempty"`
	Cert     []byte          `json:"cert"`
	Key      []byte          `json:"key"`
	At       int64           `json:"at"`
}

// StandbyTLSDir holds the main node's certificate on the other members.
func StandbyTLSDir() string { return filepath.Join(config.StateDir, "standby-tls") }

// MakeBundle collects the bundle on the main node, leaving out the member's own entry (on
// the member, that node is "local").
func MakeBundle(store *nodes.Store, memberID, mainName string) (Bundle, error) {
	b := Bundle{MainName: mainName, At: time.Now().Unix(), Nodes: []nodes.Node{}}
	for _, n := range store.List() {
		if n.ID != memberID {
			b.Nodes = append(b.Nodes, n)
		}
	}
	if t, err := os.ReadFile(filepath.Join(config.StateDir, "tokens.json")); err == nil {
		b.Tokens = t
	}
	var err error
	if b.Cert, err = os.ReadFile(filepath.Join(config.StateDir, "tls", "cert.pem")); err != nil {
		return b, err
	}
	if b.Key, err = os.ReadFile(filepath.Join(config.StateDir, "tls", "key.pem")); err != nil {
		return b, err
	}
	return b, nil
}

func writeFile(p string, b []byte, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, b, mode); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

// ReceiveBundle stores a bundle on a member. Its dashboard reads it when it starts serving
// the virtual IP. The cluster settings in it are the caller's to store.
func ReceiveBundle(b Bundle) error {
	c := Load()
	if !c.Enabled || c.Role == RoleMain {
		return errors.New("this node isn't a member of the virtual IP")
	}
	if len(b.Cert) == 0 || len(b.Key) == 0 {
		return errors.New("the bundle has no certificate")
	}
	list, _ := json.MarshalIndent(b.Nodes, "", "  ")
	if err := writeFile(filepath.Join(config.StateDir, "nodes.json"), list, 0o600); err != nil {
		return err
	}
	if len(b.Tokens) > 0 {
		if err := writeFile(filepath.Join(config.StateDir, "tokens.json"), b.Tokens, 0o600); err != nil {
			return err
		}
	}
	if err := writeFile(filepath.Join(StandbyTLSDir(), "cert.pem"), b.Cert, 0o644); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(StandbyTLSDir(), "key.pem"), b.Key, 0o600); err != nil {
		return err
	}
	c.MainName, c.MainAddr = b.MainName, b.MainAddr
	c.SyncedAt = b.At
	return save(c)
}
