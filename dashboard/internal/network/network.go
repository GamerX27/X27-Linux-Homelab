// Package network sets a node's IPv4 address and DNS through NetworkManager. Every change
// is guarded by an NM checkpoint: unless it's confirmed within RevertAfter, NetworkManager
// itself puts the old settings back, so a wrong address can't lock the node out for good.
package network

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

// RevertAfter is how long a change has to be confirmed before NetworkManager undoes it.
const RevertAfter = 120 * time.Second

// Conn is a NetworkManager connection: its configured IPv4 settings and what's live now.
type Conn struct {
	UUID          string   `json:"uuid"`
	Name          string   `json:"name"`
	Device        string   `json:"device"`
	Type          string   `json:"type"`
	Method        string   `json:"method"` // auto (DHCP) or manual (static)
	Addresses     []string `json:"addresses"`
	Gateway       string   `json:"gateway"`
	DNS           []string `json:"dns"`
	IgnoreAutoDNS bool     `json:"ignoreAutoDns"`
	Current       Live     `json:"current"`
}

type Live struct {
	Addresses []string `json:"addresses"`
	Gateway   string   `json:"gateway"`
	DNS       []string `json:"dns"`
}

// Pending is a change waiting to be kept.
type Pending struct {
	Checkpoint string `json:"-"`
	Deadline   int64  `json:"deadline"` // unix seconds
	UUID       string `json:"uuid"`
	Name       string `json:"name"`
}

var mu sync.Mutex

func pendingPath() string { return filepath.Join(config.StateDir, "network-pending.json") }

// kinds are the connection types shown; Docker's bridges and veths are left out by name.
var kinds = map[string]bool{"802-3-ethernet": true, "ethernet": true, "bond": true, "bridge": true, "vlan": true,
	"802-11-wireless": true, "wifi": true}

func skipDevice(dev string) bool {
	return dev == "" || dev == "lo" || dev == "docker0" || strings.HasPrefix(dev, "br-") || strings.HasPrefix(dev, "veth")
}

// List returns the active connections that carry the node's own addresses.
func List() ([]Conn, error) {
	if Fake {
		return fakeList(), nil
	}
	out, err := run.Cmd(10*time.Second, "nmcli", "-t", "-f", "UUID,NAME,TYPE,DEVICE", "connection", "show", "--active")
	if err != nil {
		return nil, err
	}
	list := []Conn{}
	for _, line := range strings.Split(out, "\n") {
		f := splitTerse(line)
		if len(f) != 4 || !kinds[f[2]] || skipDevice(f[3]) {
			continue
		}
		c := Conn{UUID: f[0], Name: f[1], Type: f[2], Device: f[3]}
		detail, err := run.Cmd(10*time.Second, "nmcli", "-t", "-f", "ipv4,IP4", "connection", "show", c.UUID)
		if err != nil {
			return nil, err
		}
		parseDetail(&c, detail)
		list = append(list, c)
	}
	return list, nil
}

// splitTerse splits a line of `nmcli -t` output on the colons nmcli didn't escape.
func splitTerse(line string) []string {
	var out []string
	var cur strings.Builder
	for i := 0; i < len(line); i++ {
		switch {
		case line[i] == '\\' && i+1 < len(line):
			i++
			cur.WriteByte(line[i])
		case line[i] == ':':
			out = append(out, cur.String())
			cur.Reset()
		default:
			cur.WriteByte(line[i])
		}
	}
	return append(out, cur.String())
}

func list(v string) []string {
	out := []string{}
	for _, s := range strings.Split(v, ",") {
		if s = strings.TrimSpace(s); s != "" && s != "--" {
			out = append(out, s)
		}
	}
	return out
}

// parseDetail fills c from `nmcli -t -f ipv4,IP4 connection show <uuid>`.
func parseDetail(c *Conn, out string) {
	c.Addresses, c.DNS, c.Current.Addresses, c.Current.DNS = []string{}, []string{}, []string{}, []string{}
	for _, line := range strings.Split(out, "\n") {
		key, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		v = strings.ReplaceAll(strings.TrimSpace(v), `\:`, ":")
		if v == "--" {
			v = ""
		}
		if i := strings.IndexByte(key, '['); i >= 0 {
			key = key[:i]
		}
		switch key {
		case "ipv4.method":
			c.Method = v
		case "ipv4.addresses":
			c.Addresses = list(v)
		case "ipv4.gateway":
			c.Gateway = v
		case "ipv4.dns":
			c.DNS = list(v)
		case "ipv4.ignore-auto-dns":
			c.IgnoreAutoDNS = v == "yes"
		case "IP4.ADDRESS":
			c.Current.Addresses = append(c.Current.Addresses, list(v)...)
		case "IP4.GATEWAY":
			c.Current.Gateway = v
		case "IP4.DNS":
			c.Current.DNS = append(c.Current.DNS, list(v)...)
		}
	}
}

// Validate checks a change and normalizes it: DHCP drops static addresses and the gateway.
func Validate(c *Conn) error {
	switch c.Method {
	case "auto":
		c.Addresses, c.Gateway = []string{}, ""
	case "manual":
		if len(c.Addresses) == 0 {
			return errors.New("a static setup needs an address, like 192.168.1.10/24")
		}
	default:
		return errors.New("choose DHCP or static")
	}
	var prefixes []netip.Prefix
	for i, a := range c.Addresses {
		p, err := netip.ParsePrefix(strings.TrimSpace(a))
		if err != nil || !p.Addr().Is4() {
			return fmt.Errorf("%q isn't an IPv4 address with a prefix, like 192.168.1.10/24", a)
		}
		if p.Bits() < 8 || p.Bits() > 32 {
			return fmt.Errorf("the prefix of %s should be between /8 and /32", a)
		}
		prefixes = append(prefixes, p)
		c.Addresses[i] = p.String()
	}
	if c.Gateway = strings.TrimSpace(c.Gateway); c.Gateway != "" {
		g, err := netip.ParseAddr(c.Gateway)
		if err != nil || !g.Is4() {
			return fmt.Errorf("%q isn't an IPv4 gateway address", c.Gateway)
		}
		in := false
		for _, p := range prefixes {
			in = in || (p.Contains(g) && g != p.Addr())
		}
		if !in {
			return fmt.Errorf("the gateway %s isn't in the network of the address", g)
		}
	}
	if len(c.DNS) > 3 {
		return errors.New("use at most 3 DNS servers")
	}
	for i, d := range c.DNS {
		a, err := netip.ParseAddr(strings.TrimSpace(d))
		if err != nil || !a.Is4() {
			return fmt.Errorf("%q isn't an IPv4 DNS server address", d)
		}
		c.DNS[i] = a.String()
	}
	if c.Method == "manual" {
		c.IgnoreAutoDNS = false
	} else {
		c.IgnoreAutoDNS = len(c.DNS) > 0
	}
	return nil
}

// modifyArgs is the nmcli command that writes c's IPv4 settings.
func modifyArgs(c Conn) []string {
	yes := "no"
	if c.IgnoreAutoDNS {
		yes = "yes"
	}
	return []string{"connection", "modify", c.UUID,
		"ipv4.method", c.Method,
		"ipv4.addresses", strings.Join(c.Addresses, ","),
		"ipv4.gateway", c.Gateway,
		"ipv4.dns", strings.Join(c.DNS, ","),
		"ipv4.ignore-auto-dns", yes}
}

const nm, nmPath = "org.freedesktop.NetworkManager", "/org/freedesktop/NetworkManager"

var objRe = regexp.MustCompile(`^o "([^"]+)"$`)

// busObject runs a busctl call on NetworkManager that returns one object path.
func busObject(args ...string) (string, error) {
	out, err := run.Cmd(15*time.Second, "busctl", append([]string{"call", nm, nmPath, nm}, args...)...)
	if err != nil {
		return "", err
	}
	m := objRe.FindStringSubmatch(out)
	if m == nil {
		return "", fmt.Errorf("unexpected answer from NetworkManager: %s", out)
	}
	return m[1], nil
}

// Apply changes a connection's IPv4 settings. The change takes effect a second after the
// reply goes out and is undone by NetworkManager unless Confirm runs within RevertAfter.
func Apply(c Conn) (Pending, error) {
	if err := Validate(&c); err != nil {
		return Pending{}, err
	}
	mu.Lock()
	defer mu.Unlock()
	if p := pendingLocked(); p != nil {
		return Pending{}, errors.New("keep or revert the last network change first")
	}
	if Fake {
		return fakeApply(c)
	}
	conns, err := List()
	if err != nil {
		return Pending{}, err
	}
	var cur *Conn
	for i := range conns {
		if conns[i].UUID == c.UUID {
			cur = &conns[i]
		}
	}
	if cur == nil {
		return Pending{}, errors.New("there's no active connection with that ID")
	}
	c.Name, c.Device = cur.Name, cur.Device

	dev, err := busObject("GetDeviceByIpIface", "s", c.Device)
	if err != nil {
		return Pending{}, err
	}
	// Flag 1 (destroy all) clears checkpoints left over from an earlier, crashed attempt.
	cp, err := busObject("CheckpointCreate", "aouu", "1", dev, fmt.Sprint(int(RevertAfter.Seconds())), "1")
	if err != nil {
		return Pending{}, err
	}
	if _, err := run.Cmd(15*time.Second, "nmcli", modifyArgs(c)...); err != nil {
		run.Cmd(15*time.Second, "busctl", "call", nm, nmPath, nm, "CheckpointRollback", "o", cp)
		return Pending{}, err
	}
	p := Pending{Checkpoint: cp, Deadline: time.Now().Add(RevertAfter).Unix(), UUID: c.UUID, Name: c.Name}
	if err := savePending(&p); err != nil {
		run.Cmd(15*time.Second, "busctl", "call", nm, nmPath, nm, "CheckpointRollback", "o", cp)
		return Pending{}, err
	}
	go func() {
		time.Sleep(time.Second)
		run.Cmd(60*time.Second, "nmcli", "connection", "up", c.UUID)
	}()
	return p, nil
}

// PendingChange is the change waiting to be kept, or nil.
func PendingChange() *Pending {
	mu.Lock()
	defer mu.Unlock()
	return pendingLocked()
}

func pendingLocked() *Pending {
	if Fake {
		return fakePending()
	}
	b, err := os.ReadFile(pendingPath())
	if err != nil {
		return nil
	}
	var s saved
	if json.Unmarshal(b, &s) != nil || time.Now().Unix() >= s.Deadline {
		os.Remove(pendingPath())
		return nil
	}
	s.Pending.Checkpoint = s.Checkpoint
	return &s.Pending
}

// saved is Pending as kept on disk, with the checkpoint the API doesn't show.
type saved struct {
	Pending
	Checkpoint string `json:"checkpoint"`
}

func savePending(p *Pending) error {
	if p == nil {
		err := os.Remove(pendingPath())
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	b, _ := json.Marshal(saved{*p, p.Checkpoint})
	if err := os.MkdirAll(config.StateDir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(pendingPath(), b, 0o600)
}

// Confirm keeps the pending change (keep) or puts the old settings back now (revert).
func Confirm(keep bool) error {
	mu.Lock()
	defer mu.Unlock()
	p := pendingLocked()
	if p == nil {
		return errors.New("there's no network change waiting; it may already have been reverted")
	}
	if Fake {
		return fakeConfirm(keep)
	}
	method := "CheckpointDestroy"
	if !keep {
		method = "CheckpointRollback"
	}
	if _, err := run.Cmd(15*time.Second, "busctl", "call", nm, nmPath, nm, method, "o", p.Checkpoint); err != nil {
		return err
	}
	return savePending(nil)
}
