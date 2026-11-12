// Package vip is one node's half of the cluster's virtual IP: a floating IPv4 address (VRRP,
// through keepalived) that one of two or more member nodes holds, the one with the highest
// priority whose dashboard answers. The cluster settings (internal/cluster) say which nodes
// are members; the main node works out each member's half and sends it here. The main node
// also copies what the dashboard needs to every member (paired nodes, API tokens, its
// certificate), so whichever member holds the address can serve the full dashboard there.
package vip

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

const (
	// RoleMain is the main node itself: it serves its own dashboard and never stands in.
	RoleMain = "main"
	// RoleStandby is any other member: while it holds the address it stands in for the main
	// node's dashboard there.
	RoleStandby = "standby"

	ConfPath = "/etc/keepalived/keepalived.conf"
	unit     = "keepalived.service"

	// ReleasePath makes this node give the address up on purpose (dashboard-check fails
	// while it exists), for a controlled switchover to another member.
	ReleasePath = "/run/dashboard/vip-release"
)

// Config is this node's half; every member keeps its own.
type Config struct {
	Enabled   bool     `json:"enabled"`
	Address   string   `json:"address"`  // the floating address, like 192.168.1.50
	Prefix    int      `json:"prefix"`   // its subnet's prefix length, like 24
	Role      string   `json:"role"`     // main or standby: whether this node stands in for the dashboard
	Priority  int      `json:"priority"` // VRRP priority; the highest healthy member holds the address
	Peers     []string `json:"peers"`    // the other members' own addresses
	Nopreempt bool     `json:"nopreempt"`
	RouterID  int      `json:"routerId"`       // VRRP virtual router id, 1-255; the same on every member
	Password  string   `json:"password"`       // VRRP auth, the same on every member (at most 8 characters)
	Port      int      `json:"port"`           // the dashboard's port, for the health check
	Self      string   `json:"self,omitempty"` // this node's id in the main node's list ("local" on the main node)

	MainName string `json:"mainName,omitempty"` // on other members: the main node's name, for the banner
	MainAddr string `json:"mainAddr,omitempty"` // on other members: where the main node's cluster API answers
	SyncedAt int64  `json:"syncedAt,omitempty"` // on other members: when the main node last copied its state

	// From before the cluster settings: one main node and one standby.
	Peer    string `json:"peer,omitempty"`
	Standby string `json:"standby,omitempty"`
}

// upgrade fills the member fields from an old main/standby pair.
func upgrade(c Config) Config {
	if len(c.Peers) == 0 && c.Peer != "" {
		c.Peers = []string{c.Peer}
	}
	if c.Priority == 0 && c.Enabled {
		c.Priority = 100
		if c.Role == RoleMain {
			c.Priority = 150
		}
	}
	return c
}

var (
	mu     sync.Mutex
	cached *Config
)

func path() string { return filepath.Join(config.StateDir, "vip.json") }

// Load reads this node's settings; without any, it's off.
func Load() Config {
	mu.Lock()
	defer mu.Unlock()
	if cached != nil {
		return *cached
	}
	var c Config
	if b, err := os.ReadFile(path()); err == nil {
		json.Unmarshal(b, &c)
	}
	c = upgrade(c)
	cached = &c
	return c
}

func save(c Config) error {
	b, _ := json.MarshalIndent(c, "", "  ")
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
	mu.Lock()
	cached = &c
	mu.Unlock()
	return nil
}

// IsVIP reports whether ip is the floating address, so lists of the node's own addresses can
// leave it out: it comes and goes with whoever holds it.
func IsVIP(ip string) bool {
	c := Load()
	return c.Enabled && c.Address != "" && strings.TrimSpace(ip) == c.Address
}

// NewSecret makes a VRRP password and router id for a new setup.
func NewSecret() (string, int) {
	b := make([]byte, 4)
	rand.Read(b)
	return hex.EncodeToString(b), int(b[0])%254 + 1
}

// Iface is the interface with an address in the floating address's subnet.
type Iface struct {
	Name string
	IP   string // this node's own address there
}

// FindInterface picks the interface whose own IPv4 subnet holds addr.
func FindInterface(addr string, prefix int) (Iface, error) {
	ip := net.ParseIP(addr).To4()
	if ip == nil {
		return Iface{}, errors.New("the virtual IP must be an IPv4 address, like 192.168.1.50")
	}
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		if i.Flags&net.FlagUp == 0 || i.Flags&net.FlagLoopback != 0 || strings.HasPrefix(i.Name, "docker") ||
			strings.HasPrefix(i.Name, "br-") || strings.HasPrefix(i.Name, "veth") {
			continue
		}
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			ipn, ok := a.(*net.IPNet)
			if !ok || ipn.IP.To4() == nil || ipn.IP.Equal(ip) {
				continue
			}
			ones, _ := ipn.Mask.Size()
			if prefix > 0 && ones != prefix {
				continue
			}
			if ipn.Contains(ip) {
				return Iface{Name: i.Name, IP: ipn.IP.String()}, nil
			}
		}
	}
	return Iface{}, fmt.Errorf("this node has no address in %s/%d; the virtual IP must be in the same subnet as both nodes", addr, prefix)
}

// Holds reports whether this node has the floating address right now. With Fake, it holds
// it while the file vip-fake-holds is in the state folder (and it isn't releasing it).
func Holds() bool {
	c := Load()
	if !c.Enabled || c.Address == "" {
		return false
	}
	if Fake {
		_, err := os.Stat(filepath.Join(config.StateDir, "vip-fake-holds"))
		return err == nil && !Releasing()
	}
	return hasAddress(c.Address)
}

func hasAddress(addr string) bool {
	ifaces, _ := net.Interfaces()
	for _, i := range ifaces {
		addrs, _ := i.Addrs()
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && ipn.IP.String() == addr {
				return true
			}
		}
	}
	return false
}

// Validate checks the settings before they're applied.
func Validate(c Config) error {
	if !c.Enabled {
		return nil
	}
	ip := net.ParseIP(c.Address).To4()
	if ip == nil {
		return errors.New("the virtual IP must be an IPv4 address, like 192.168.1.50")
	}
	if c.Prefix < 8 || c.Prefix > 30 {
		return errors.New("the prefix must be between 8 and 30, like 24")
	}
	if len(c.Peers) == 0 {
		return errors.New("the virtual IP needs at least two member nodes")
	}
	for _, p := range c.Peers {
		if net.ParseIP(p).To4() == nil {
			return fmt.Errorf("the other members' addresses must be IPv4 addresses (%q isn't)", p)
		}
	}
	if c.Priority < 1 || c.Priority > 254 {
		return errors.New("the priority must be between 1 and 254")
	}
	if c.Role != RoleMain && c.Role != RoleStandby {
		return errors.New("role must be main or standby")
	}
	if c.RouterID < 1 || c.RouterID > 255 || c.Password == "" || len(c.Password) > 8 {
		return errors.New("bad router id or password")
	}
	return nil
}

var confTmpl = template.Must(template.New("k").Parse(`# Written by the dashboard (Cluster → Virtual IP). Changes here are overwritten.
global_defs {
    router_id dashboard_{{.Role}}
    enable_script_security
    script_user root
}

# The dashboard answers on its port: if it stops (or /run/dashboard/vip-release exists),
# this node lets the address go.
vrrp_script dashboard {
    script "/usr/libexec/keepalived/dashboard-check {{.Port}}"
    interval 2
    fall 3
    rise 2
}

vrrp_instance dashboard {
    state BACKUP
    interface {{.Iface}}
    virtual_router_id {{.RouterID}}
    priority {{.Priority}}
    advert_int 1
{{- if .Nopreempt}}
    # No automatic failback: the address stays where it is until it's moved on purpose
    # (Cluster → Move virtual IP), so apps never restart on an out-of-date copy.
    nopreempt
{{- else}}
    preempt_delay 30
{{- end}}
    unicast_src_ip {{.Own}}
    unicast_peer {
{{- range .Peers}}
        {{.}}
{{- end}}
    }
    authentication {
        auth_type PASS
        auth_pass {{.Password}}
    }
    virtual_ipaddress {
        {{.Address}}/{{.Prefix}} dev {{.Iface}}
    }
    track_script {
        dashboard
    }
    # A member that isn't the main node serves the dashboard only while it holds the address.
    notify_master "/usr/libexec/keepalived/dashboard-notify {{.Role}} master"
    notify_backup "/usr/libexec/keepalived/dashboard-notify {{.Role}} backup"
    notify_fault "/usr/libexec/keepalived/dashboard-notify {{.Role}} fault"
}
`))

// Render is keepalived.conf for c on the interface it runs on.
func Render(c Config, iface Iface) (string, error) {
	c = upgrade(c)
	role := c.Role
	if role != RoleMain {
		role = RoleStandby
	}
	var sb strings.Builder
	err := confTmpl.Execute(&sb, map[string]any{"Role": role, "Port": c.Port, "Iface": iface.Name, "RouterID": c.RouterID,
		"Priority": c.Priority, "Nopreempt": c.Nopreempt, "Own": iface.IP, "Peers": c.Peers, "Password": c.Password,
		"Address": c.Address, "Prefix": c.Prefix})
	return sb.String(), err
}

// Fake skips keepalived and systemd (development).
var Fake bool

// Apply saves c and sets keepalived up for it, or turns it off.
func Apply(c Config) error {
	if err := Validate(c); err != nil {
		return err
	}
	c = upgrade(c)
	c.Peer, c.Standby = "", ""
	if !c.Enabled {
		if !Fake {
			run.Cmd(30*time.Second, "systemctl", "disable", "--now", unit)
		}
		os.Remove(releasePath())
		return save(c)
	}
	iface, err := FindInterface(c.Address, c.Prefix)
	if err != nil && Fake {
		iface, err = Iface{Name: "fake0", IP: "127.0.0.1"}, nil
	}
	if err != nil {
		return err
	}
	for _, p := range c.Peers {
		if iface.IP == p && !Fake {
			return errors.New("one of the other members' addresses is this node's own")
		}
	}
	conf, err := Render(c, iface)
	if err != nil {
		return err
	}
	if Fake {
		return save(c)
	}
	if _, err := exec.LookPath("keepalived"); err != nil {
		return errors.New("keepalived isn't installed on this node; update it to a newer image first")
	}
	if err := os.MkdirAll(filepath.Dir(ConfPath), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(ConfPath, []byte(conf), 0o600); err != nil {
		return err
	}
	if err := save(c); err != nil {
		return err
	}
	if out, err := run.Cmd(30*time.Second, "systemctl", "enable", unit); err != nil {
		return fmt.Errorf("enabling keepalived: %v %s", err, out)
	}
	if out, err := run.Cmd(30*time.Second, "systemctl", "restart", unit); err != nil {
		return fmt.Errorf("starting keepalived: %v %s", err, out)
	}
	return nil
}

// Refresh rewrites keepalived.conf when what this dashboard would write differs from it
// (an update changed the template, or the interface's name changed), and restarts
// keepalived. The dashboard calls it when it starts.
func Refresh() error {
	c := Load()
	if !c.Enabled || Fake {
		return nil
	}
	iface, err := FindInterface(c.Address, c.Prefix)
	if err != nil {
		return err
	}
	conf, err := Render(c, iface)
	if err != nil {
		return err
	}
	if old, err := os.ReadFile(ConfPath); err == nil && string(old) == conf {
		return nil
	}
	log.Printf("virtual IP: updating %s", ConfPath)
	if err := os.WriteFile(ConfPath, []byte(conf), 0o600); err != nil {
		return err
	}
	if out, err := run.Cmd(30*time.Second, "systemctl", "restart", unit); err != nil {
		return fmt.Errorf("restarting keepalived: %v %s", err, out)
	}
	return nil
}

// Status is what the Cluster page shows for one member.
type Status struct {
	Config
	Password  string `json:"-"`
	Holds     bool   `json:"holds"`               // this node has the address now
	Active    bool   `json:"active"`              // keepalived runs
	Releasing bool   `json:"releasing,omitempty"` // giving the address up on purpose
	Iface     string `json:"iface,omitempty"`
	Error     string `json:"error,omitempty"`
}

func releasePath() string {
	if Fake {
		return filepath.Join(config.StateDir, "vip-release")
	}
	return ReleasePath
}

// Release makes this node give the address up (on) or compete for it again (off).
func Release(on bool) error {
	p := releasePath()
	if !on {
		if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
}

// Releasing reports whether this node is giving the address up on purpose.
func Releasing() bool {
	_, err := os.Stat(releasePath())
	return err == nil
}

func GetStatus() Status {
	c := Load()
	s := Status{Config: c, Holds: Holds(), Releasing: Releasing()}
	if !c.Enabled {
		return s
	}
	if iface, err := FindInterface(c.Address, c.Prefix); err == nil {
		s.Iface = iface.Name
	} else if Fake {
		s.Iface = "fake0"
	} else {
		s.Error = err.Error()
	}
	if Fake {
		s.Active = true
	} else if out, _ := run.Cmd(30*time.Second, "systemctl", "is-active", unit); strings.TrimSpace(out) == "active" {
		s.Active = true
	}
	return s
}
