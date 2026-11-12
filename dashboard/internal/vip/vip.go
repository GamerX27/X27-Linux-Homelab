// Package vip gives the dashboard one address that always reaches it: a floating IPv4
// address (VRRP, through keepalived) that the main node holds, and that a standby node takes
// over when the main node or its dashboard goes down. The main node copies what the
// dashboard needs to the standby (paired nodes, API tokens, its certificate), so the standby
// can serve the full dashboard on that address. When the main node is back, it takes the
// address back.
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
	RoleMain    = "main"
	RoleStandby = "standby"

	ConfPath = "/etc/keepalived/keepalived.conf"
	unit     = "keepalived.service"
)

// Config is this node's half; the main node and the standby each keep their own.
type Config struct {
	Enabled  bool   `json:"enabled"`
	Address  string `json:"address"` // the floating address, like 192.168.1.50
	Prefix   int    `json:"prefix"`  // its subnet's prefix length, like 24
	Role     string `json:"role"`    // main or standby
	Peer     string `json:"peer"`    // the other node's own address
	Standby  string `json:"standby,omitempty"`
	RouterID int    `json:"routerId"` // VRRP virtual router id, 1-255; the same on both
	Password string `json:"password"` // VRRP auth, the same on both (at most 8 characters)
	Port     int    `json:"port"`     // the dashboard's port, for the health check

	MainName string `json:"mainName,omitempty"` // on the standby: the main node's name, for its banner
	SyncedAt int64  `json:"syncedAt,omitempty"` // on the standby: when the main node last copied its state
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

// Holds reports whether this node has the floating address right now.
func Holds() bool {
	c := Load()
	if !c.Enabled || c.Address == "" {
		return false
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
	if net.ParseIP(c.Peer).To4() == nil {
		return errors.New("the other node's address must be an IPv4 address")
	}
	if c.Role != RoleMain && c.Role != RoleStandby {
		return errors.New("role must be main or standby")
	}
	if c.RouterID < 1 || c.RouterID > 255 || c.Password == "" || len(c.Password) > 8 {
		return errors.New("bad router id or password")
	}
	return nil
}

var confTmpl = template.Must(template.New("k").Parse(`# Written by the dashboard (System → Settings → Virtual IP). Changes here are overwritten.
global_defs {
    router_id dashboard_{{.Role}}
    enable_script_security
    script_user root
}

# The dashboard answers on its port: if it stops, this node lets the address go.
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
{{- if eq .Role "main"}}
    preempt_delay 30
{{- end}}
    unicast_src_ip {{.Own}}
    unicast_peer {
        {{.Peer}}
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
{{- if eq .Role "standby"}}
    # The standby serves the full dashboard only while it holds the address.
    notify_master "/usr/libexec/keepalived/dashboard-notify"
    notify_backup "/usr/libexec/keepalived/dashboard-notify"
{{- end}}
}
`))

// Render is keepalived.conf for c on the interface it runs on.
func Render(c Config, iface Iface) (string, error) {
	pri := 100
	if c.Role == RoleMain {
		pri = 150
	}
	var sb strings.Builder
	err := confTmpl.Execute(&sb, map[string]any{"Role": c.Role, "Port": c.Port, "Iface": iface.Name, "RouterID": c.RouterID,
		"Priority": pri, "Own": iface.IP, "Peer": c.Peer, "Password": c.Password, "Address": c.Address, "Prefix": c.Prefix})
	return sb.String(), err
}

// Fake skips keepalived and systemd (development).
var Fake bool

// Apply saves c and sets keepalived up for it, or turns it off.
func Apply(c Config) error {
	if err := Validate(c); err != nil {
		return err
	}
	if !c.Enabled {
		if !Fake {
			run.Cmd(30*time.Second, "systemctl", "disable", "--now", unit)
		}
		return save(c)
	}
	iface, err := FindInterface(c.Address, c.Prefix)
	if err != nil {
		return err
	}
	if iface.IP == c.Peer && !Fake {
		return errors.New("the other node's address is this node's own")
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

// Status is what the Settings card shows.
type Status struct {
	Config
	Password string `json:"-"`
	Holds    bool   `json:"holds"`  // this node has the address now
	Active   bool   `json:"active"` // keepalived runs
	Iface    string `json:"iface,omitempty"`
	Error    string `json:"error,omitempty"`
}

func GetStatus() Status {
	c := Load()
	s := Status{Config: c, Holds: Holds()}
	if !c.Enabled {
		return s
	}
	if iface, err := FindInterface(c.Address, c.Prefix); err == nil {
		s.Iface = iface.Name
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
