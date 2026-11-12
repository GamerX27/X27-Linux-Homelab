// Package cluster holds the settings for the whole fleet rather than one node: the virtual
// IP and its member nodes, shares mounted on several nodes, and high-availability apps
// that run on whichever member holds the virtual IP. The main node owns the settings
// (cluster.json in its state folder); every member of the virtual IP gets a copy, without
// passwords, with the state the main node already sends it (see internal/vip).
package cluster

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/storage"
)

// LocalID is the main node in member and share lists.
const LocalID = "local"

// Data modes of an HA app.
const (
	DataShared     = "shared"     // all its data is on cluster shares: failover is instant
	DataReplicated = "replicated" // its folder and volumes are copied to the other members on a timer
	DataSynced     = "synced"     // like replicated, but only what changed, every few seconds
)

// Copies reports whether an app's data is copied between the members (not on a share).
func (a HAApp) Copies() bool { return a.Data == DataReplicated || a.Data == DataSynced }

// DefaultSyncInterval is how often a synced app sends its changes unless set.
const DefaultSyncInterval = 30

// Member is one node of the virtual IP. The first member has the highest priority.
type Member struct {
	NodeID   string `json:"nodeId"`
	Priority int    `json:"priority"`
}

// VIPConfig is the one cluster virtual IP.
type VIPConfig struct {
	Enabled  bool     `json:"enabled"`
	Address  string   `json:"address"`
	Prefix   int      `json:"prefix"`
	Members  []Member `json:"members"`
	Failback bool     `json:"failback"` // the first member takes the address back when it's healthy again
	RouterID int      `json:"routerId"`
	Password string   `json:"password,omitempty"`
}

// Share is one NFS or SMB share mounted at /var/mnt/<name> on every node, or on the
// nodes listed.
type Share struct {
	storage.Mount
	Nodes    []string `json:"nodes"`              // node ids; empty is every node
	Password string   `json:"password,omitempty"` // SMB; kept on the main node only, never sent back
	PassGen  int      `json:"passGen,omitempty"`  // bumped when the password changes, so nodes get the new one
}

// HAApp is a stack that runs on whichever member holds the virtual IP.
type HAApp struct {
	Name     string   `json:"name"`
	Owner    string   `json:"owner"`              // whose ~/docker holds it, on every member
	Data     string   `json:"data"`               // shared or replicated
	Interval int      `json:"interval,omitempty"` // replicated: seconds between copies
	VIPOnly  bool     `json:"vipOnly"`            // published ports only on the virtual IP
	Shares   []string `json:"shares,omitempty"`   // cluster shares it uses; a shared app's lock is on the first
	Created  int64    `json:"created"`
}

// State is everything in cluster.json.
type State struct {
	VIP    VIPConfig `json:"vip"`
	Shares []Share   `json:"shares"`
	Apps   []HAApp   `json:"apps"`
	// Secret lets the members reach the main node's cluster API (for replicas and takeover
	// checks); it goes to the members with the rest.
	Secret    string `json:"secret,omitempty"`
	Rev       int64  `json:"rev"`
	UpdatedAt int64  `json:"updatedAt"`
}

// DefaultInterval is how often a replicated app is copied unless set.
const DefaultInterval = 15 * 60

var (
	mu     sync.Mutex
	cached *State
)

func path() string { return filepath.Join(config.StateDir, "cluster.json") }

func normalize(s *State) {
	if s.Shares == nil {
		s.Shares = []Share{}
	}
	if s.Apps == nil {
		s.Apps = []HAApp{}
	}
	if s.VIP.Members == nil {
		s.VIP.Members = []Member{}
	}
	for i := range s.Shares {
		if s.Shares[i].Nodes == nil {
			s.Shares[i].Nodes = []string{}
		}
	}
}

// Load reads the cluster settings; without any, nothing is set up.
func Load() State {
	mu.Lock()
	defer mu.Unlock()
	if cached == nil {
		var s State
		if b, err := os.ReadFile(path()); err == nil {
			json.Unmarshal(b, &s)
		}
		normalize(&s)
		cached = &s
	}
	return cached.clone()
}

func (s State) clone() State {
	b, _ := json.Marshal(s)
	var c State
	json.Unmarshal(b, &c)
	normalize(&c)
	return c
}

func saveLocked(s State) error {
	b, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(config.StateDir, 0o700); err != nil {
		return err
	}
	tmp := path() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, path()); err != nil {
		return err
	}
	cached = &s
	return nil
}

// Update changes the settings on the main node: fn edits a copy, which is kept only when
// fn returns nil. Every change bumps Rev.
func Update(fn func(s *State) error) (State, error) {
	mu.Lock()
	defer mu.Unlock()
	var s State
	if cached != nil {
		s = cached.clone()
	} else {
		if b, err := os.ReadFile(path()); err == nil {
			json.Unmarshal(b, &s)
		}
		normalize(&s)
	}
	if err := fn(&s); err != nil {
		return s, err
	}
	if s.Secret == "" {
		s.Secret = newSecret()
	}
	s.Rev++
	s.UpdatedAt = time.Now().Unix()
	normalize(&s)
	return s, saveLocked(s)
}

// Receive stores the copy the main node sent (on a member).
func Receive(raw []byte) error {
	var s State
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("the cluster settings in the copy aren't valid: %w", err)
	}
	normalize(&s)
	mu.Lock()
	defer mu.Unlock()
	return saveLocked(s)
}

func newSecret() string {
	b := make([]byte, 24)
	rand.Read(b)
	return "x27cl_" + hex.EncodeToString(b)
}

// ForMembers is the copy a member gets: no passwords.
func (s State) ForMembers() State {
	c := s.clone()
	c.VIP.Password = ""
	for i := range c.Shares {
		c.Shares[i].HasPassword = c.Shares[i].Password != "" || c.Shares[i].HasPassword
		c.Shares[i].Password = ""
	}
	return c
}

// Public is what the browser sees: no passwords and no secret.
func (s State) Public() State {
	c := s.ForMembers()
	c.Secret = ""
	return c
}

// MemberIDs are the virtual IP's members in priority order.
func (s State) MemberIDs() []string { return s.VIP.MemberIDs() }

func (v VIPConfig) MemberIDs() []string {
	out := []string{}
	for _, m := range v.Members {
		out = append(out, m.NodeID)
	}
	return out
}

func (s State) IsMember(id string) bool { return slices.Contains(s.MemberIDs(), id) }

func (s State) App(name string) (HAApp, bool) {
	for _, a := range s.Apps {
		if a.Name == name {
			return a, true
		}
	}
	return HAApp{}, false
}

func (s State) Share(name string) (Share, bool) {
	for _, sh := range s.Shares {
		if sh.Name == name {
			return sh, true
		}
	}
	return Share{}, false
}

// Targets are the nodes a share goes on, out of all of them.
func (sh Share) Targets(all []string) []string {
	if len(sh.Nodes) == 0 {
		return slices.Clone(all)
	}
	var out []string
	for _, id := range all {
		if slices.Contains(sh.Nodes, id) {
			out = append(out, id)
		}
	}
	return out
}

// OnAll reports whether a share goes on every one of ids.
func (sh Share) OnAll(ids []string) bool {
	if len(sh.Nodes) == 0 {
		return true
	}
	for _, id := range ids {
		if !slices.Contains(sh.Nodes, id) {
			return false
		}
	}
	return true
}

// Priorities fills in each member's priority from its place: 150, 140, 130…
func Priorities(members []Member) []Member {
	out := slices.Clone(members)
	for i := range out {
		out[i].Priority = max(150-10*i, 1)
	}
	return out
}

// ValidateApp checks an HA app's settings before it's kept.
func ValidateApp(a *HAApp) error {
	if !docker.ValidStackName(a.Name) {
		return errors.New("pick a stack")
	}
	switch a.Data {
	case DataShared:
		a.Interval = 0
	case DataReplicated:
		if a.Interval == 0 {
			a.Interval = DefaultInterval
		}
		if a.Interval < 60 || a.Interval > 7*24*3600 {
			return errors.New("copy a replicated app every 1 minute to 7 days")
		}
	case DataSynced:
		if a.Interval == 0 {
			a.Interval = DefaultSyncInterval
		}
		if a.Interval < 10 || a.Interval > 3600 {
			return errors.New("sync an app every 10 seconds to 1 hour")
		}
	default:
		return errors.New("choose where its data is: shared, replicated or synced")
	}
	return nil
}

// Self maps a node id from the main node's list to how this node calls it: on the main
// node (self "local") ids are as they are; on another member its own id is "local" and
// the main node is reached through MainPeer.
func Self(self, id string) string {
	if self == "" || self == LocalID {
		return id
	}
	if id == self {
		return LocalID
	}
	if id == LocalID {
		return MainPeer
	}
	return id
}

// MainPeer is how a member that isn't the main node names the main node.
const MainPeer = "main"
