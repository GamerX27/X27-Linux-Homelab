package backup

import (
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/apitokens"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/features"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/network"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/nodes"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/storage"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/system"
)

// ManifestVersion is server.json's format; an import refuses newer ones.
const ManifestVersion = 1

// Manifest is server.json in an export: the server's own settings, each part optional.
type Manifest struct {
	Version   int            `json:"version"`
	Host      string         `json:"host"`
	Taken     int64          `json:"taken"`
	Owner     *ManifestOwner `json:"owner,omitempty"` // whose ~/docker the apps came from
	Apps      []ManifestApp  `json:"apps"`
	Dashboard *DashboardPart `json:"dashboard,omitempty"`
	Shares    *SharesPart    `json:"shares,omitempty"`
	Updates   *UpdatesPart   `json:"updates,omitempty"`
	Network   *NetworkPart   `json:"network,omitempty"`
}

type ManifestOwner struct {
	Name string `json:"name"`
	UID  int    `json:"uid"`
	GID  int    `json:"gid"`
}

type ManifestApp struct {
	Name string `json:"name"` // the folder in ~/docker
	Data bool   `json:"data"`
}

type DashboardPart struct {
	Port    int               `json:"port,omitempty"`
	Presets string            `json:"presets,omitempty"`
	Backups *Config           `json:"backups,omitempty"`
	Nodes   []nodes.Node      `json:"nodes,omitempty"`  // main node only, with their tokens
	Tokens  []apitokens.Token `json:"tokens,omitempty"` // main node only, with their hashes
}

type SharesPart struct {
	Mounts []storage.Share `json:"mounts"`
}

type UpdatesPart struct {
	Autoupdate  *Schedule       `json:"autoupdate,omitempty"` // nil: not on this server
	GotifyURL   string          `json:"gotifyUrl,omitempty"`
	GotifyToken string          `json:"gotifyToken,omitempty"`
	Services    map[string]bool `json:"services,omitempty"`
}

// Schedule is an autoupdate schedule; Freq "off" means automatic updates are off.
type Schedule struct {
	Freq    string `json:"freq"`
	Weekday string `json:"weekday,omitempty"`
	Day     string `json:"day,omitempty"`
	Time    string `json:"time,omitempty"`
}

type NetworkPart struct {
	Hostname    string    `json:"hostname,omitempty"`
	Timezone    string    `json:"timezone,omitempty"`
	NTP         bool      `json:"ntp"`
	Connections []NetConn `json:"connections,omitempty"`
}

// NetConn is a connection's configured IPv4 settings, matched on import by name, then device.
type NetConn struct {
	Name          string   `json:"name"`
	Device        string   `json:"device"`
	Method        string   `json:"method"`
	Addresses     []string `json:"addresses"`
	Gateway       string   `json:"gateway"`
	DNS           []string `json:"dns"`
	IgnoreAutoDNS bool     `json:"ignoreAutoDns"`
}

// Parts are the server sections an export or import takes.
type Parts struct {
	Dashboard, Shares, Updates, Network bool
}

// ParseParts reads "dashboard,shares,updates,network" (any of them, comma separated or repeated).
func ParseParts(list []string) (Parts, error) {
	var p Parts
	for _, item := range list {
		for _, s := range strings.Split(item, ",") {
			switch strings.TrimSpace(s) {
			case "":
			case "dashboard":
				p.Dashboard = true
			case "shares":
				p.Shares = true
			case "updates":
				p.Updates = true
			case "network":
				p.Network = true
			default:
				return p, fmt.Errorf("unknown part %q", s)
			}
		}
	}
	return p, nil
}

func (p Parts) String() string {
	var out []string
	for _, x := range []struct {
		on   bool
		name string
	}{{p.Dashboard, "dashboard"}, {p.Shares, "shares"}, {p.Updates, "updates"}, {p.Network, "network"}} {
		if x.on {
			out = append(out, x.name)
		}
	}
	if len(out) == 0 {
		return "none"
	}
	return strings.Join(out, ", ")
}

// ServerHooks connects the import and export to what only the API layer has.
type ServerHooks struct {
	Nodes   *nodes.Store     // the main node's paired nodes; nil on a paired node
	Tokens  *apitokens.Store // the main node's API tokens; nil on a paired node
	Restart func()           // restarts the dashboard after the port or presets changed; nil in --dev
	Changed func()           // drops cached settings after the updates part was applied
	Dev     bool             // --dev: the port is never changed
}

// collect fills the server parts asked for from the same getters the UI uses. A part
// that can't be read is left out and logged.
func (m *Manager) collect(o docker.Owner, p Parts) Manifest {
	var man Manifest
	if p.Dashboard {
		d := &DashboardPart{}
		if cfg, err := config.Load(); err == nil {
			d.Port = cfg.Port
			if cfg.Mode == config.ModeMain {
				d.Presets = cfg.Presets
			}
		} else {
			log.Printf("export: reading the dashboard settings: %v", err)
		}
		c := m.Config()
		d.Backups = &c
		if m.Server.Nodes != nil {
			d.Nodes = m.Server.Nodes.List()
		}
		if m.Server.Tokens != nil {
			d.Tokens = m.Server.Tokens.Export()
		}
		man.Dashboard = d
	}
	if p.Shares {
		man.Shares = &SharesPart{Mounts: storage.Export()}
	}
	if p.Updates {
		all := features.Get()
		u := &UpdatesPart{Services: map[string]bool{}}
		if au := all.Autoupdate; au.Available {
			u.Autoupdate = &Schedule{Freq: "off"}
			if f, wd, day, clock, ok := features.ParseCalendar(au.Calendar); ok && au.Enabled {
				u.Autoupdate = &Schedule{Freq: f, Weekday: wd, Day: day, Time: clock}
			}
			if au.Gotify {
				u.GotifyURL = au.GotifyURL
				u.GotifyToken, _ = features.GotifyToken()
			}
		}
		for _, s := range all.Services {
			u.Services[s.ID] = s.Enabled
		}
		man.Updates = u
	}
	if p.Network {
		st := system.GetSettings()
		n := &NetworkPart{Hostname: st.Hostname, Timezone: st.Timezone, NTP: st.NTP}
		if conns, err := network.List(); err == nil {
			for _, c := range conns {
				n.Connections = append(n.Connections, NetConn{Name: c.Name, Device: c.Device, Method: c.Method,
					Addresses: c.Addresses, Gateway: c.Gateway, DNS: c.DNS, IgnoreAutoDNS: c.IgnoreAutoDNS})
			}
		} else {
			log.Printf("export: reading the network settings: %v", err)
		}
		man.Network = n
	}
	return man
}

// Present lists the server parts an export holds, in the order an import applies them.
func (man Manifest) Present() []string {
	var out []string
	if man.Updates != nil {
		out = append(out, "updates")
	}
	if man.Shares != nil {
		out = append(out, "shares")
	}
	if man.Dashboard != nil {
		out = append(out, "dashboard")
	}
	if man.Network != nil {
		out = append(out, "network")
	}
	return out
}

// Details describes each part in a few words, for the import preview.
func (man Manifest) Details() map[string]string {
	out := map[string]string{}
	plural := func(n int, one string) string {
		if n == 1 {
			return "1 " + one
		}
		return fmt.Sprintf("%d %ss", n, one)
	}
	if d := man.Dashboard; d != nil {
		var s []string
		if d.Port != 0 {
			s = append(s, fmt.Sprintf("port %d", d.Port))
		}
		if d.Presets != "" && d.Presets != config.DefaultPresetsRepo {
			s = append(s, "own presets")
		}
		if d.Backups != nil {
			if d.Backups.scheduled() {
				s = append(s, d.Backups.Freq+" backups of "+plural(len(d.Backups.Apps), "app"))
			} else {
				s = append(s, "backup settings")
			}
		}
		if len(d.Nodes) > 0 {
			s = append(s, plural(len(d.Nodes), "paired node"))
		}
		if len(d.Tokens) > 0 {
			s = append(s, plural(len(d.Tokens), "API token"))
		}
		out["dashboard"] = strings.Join(s, ", ")
	}
	if sh := man.Shares; sh != nil {
		var s []string
		for _, m := range sh.Mounts {
			s = append(s, m.Name+" ("+strings.ToUpper(m.Type)+")")
		}
		if len(s) == 0 {
			s = []string{"none"}
		}
		out["shares"] = strings.Join(s, ", ")
	}
	if u := man.Updates; u != nil {
		var s []string
		if a := u.Autoupdate; a != nil {
			switch a.Freq {
			case "off":
				s = append(s, "automatic updates off")
			case "weekly":
				s = append(s, "updates weekly on "+a.Weekday+" at "+a.Time)
			case "monthly":
				s = append(s, "updates monthly on day "+a.Day+" at "+a.Time)
			default:
				s = append(s, "updates "+a.Freq+" at "+a.Time)
			}
		}
		if u.GotifyURL != "" {
			s = append(s, "Gotify")
		}
		if len(u.Services) > 0 {
			s = append(s, plural(len(u.Services), "service"))
		}
		out["updates"] = strings.Join(s, ", ")
	}
	if n := man.Network; n != nil {
		s := []string{n.Hostname, n.Timezone}
		for _, c := range n.Connections {
			if c.Method == "manual" {
				s = append(s, c.Name+": static "+strings.Join(c.Addresses, ", "))
			} else {
				s = append(s, c.Name+": DHCP")
			}
		}
		out["network"] = strings.Join(slices.DeleteFunc(s, func(x string) bool { return x == "" }), ", ")
	}
	return out
}

// importUpdates sets the autoupdate schedule, Gotify and the services as exported.
// Each change that fails is logged; the error says how many did.
func (m *Manager) importUpdates(u *UpdatesPart) error {
	defer func() {
		if m.Server.Changed != nil {
			m.Server.Changed()
		}
	}()
	var failed []string
	fail := func(what string, err error) {
		m.logf("updates: %s: %v", what, err)
		failed = append(failed, what)
	}
	all := features.Get()
	if a := u.Autoupdate; a != nil && all.Autoupdate.Available {
		if a.Freq == "off" {
			if all.Autoupdate.Enabled {
				out, err := features.AutoupdateOff()
				m.logf("%s", out)
				if err != nil {
					fail("automatic updates off", err)
				} else {
					m.logf("updates: automatic updates turned off")
				}
			}
		} else if out, err := features.SetAutoupdate(a.Freq, a.Weekday, a.Day, a.Time); err != nil {
			fail("schedule", err)
		} else {
			m.logf("%s", out)
			m.logf("updates: automatic updates %s at %s", a.Freq, a.Time)
		}
	} else if a != nil {
		m.logf("updates: automatic updates aren't available on this server, skipped")
	}
	if u.GotifyURL != "" && u.GotifyToken != "" {
		if !all.Autoupdate.Available {
			m.logf("updates: Gotify needs autoupdate, which isn't on this server; skipped")
		} else if out, err := features.SetGotify(u.GotifyURL, u.GotifyToken); err != nil {
			fail("Gotify", err)
		} else {
			m.logf("%s", out)
			m.logf("updates: Gotify set to %s (a test message was sent)", u.GotifyURL)
		}
	}
	for _, s := range all.Services {
		want, ok := u.Services[s.ID]
		if !ok || want == s.Enabled {
			continue
		}
		if err := features.SetService(s.ID, want); err != nil {
			fail(s.Name, err)
		} else {
			m.logf("updates: %s turned %s", s.Name, map[bool]string{true: "on", false: "off"}[want])
		}
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, ", ") + " failed")
	}
	return nil
}

// importShares saves each mount (changing one with the same name) and mounts it.
func (m *Manager) importShares(sh *SharesPart) error {
	have := map[string]bool{}
	for _, x := range storage.List() {
		have[x.Name] = true
	}
	var failed []string
	for _, s := range sh.Mounts {
		verb := "added"
		if have[s.Name] {
			verb = "changed"
		}
		if err := storage.Save(s.Mount, s.Password, !have[s.Name]); err != nil {
			m.logf("shares: %s: %v", s.Name, err)
			failed = append(failed, s.Name)
			continue
		}
		// Save mounts it (or arms the automount) once the test mount worked.
		m.logf("shares: %s %s and mounted at %s", s.Name, verb, s.MountPoint)
	}
	if len(failed) > 0 {
		return errors.New(strings.Join(failed, ", ") + " failed")
	}
	return nil
}

// importDashboard sets the presets, the backup settings, and merges in the paired nodes
// and API tokens. It returns the exported port when it differs from this one, for the
// caller to move to last; restart is true when the dashboard must restart to load it all.
func (m *Manager) importDashboard(o docker.Owner, d *DashboardPart) (port int, restart bool, err error) {
	dev := m.Server.Dev
	var failed []string
	fail := func(what string, err error) {
		m.logf("dashboard: %s: %v", what, err)
		failed = append(failed, what)
	}
	cfg, cerr := config.Load()
	if cerr != nil {
		return 0, false, cerr
	}
	if d.Presets != "" && cfg.Mode == config.ModeMain && d.Presets != cfg.Presets {
		if err := config.SetPresets(d.Presets); err != nil {
			fail("presets", err)
		} else {
			m.logf("dashboard: presets repository set to %s", d.Presets)
			restart = !dev
		}
	}
	if d.Backups != nil {
		if err := m.Save(o, *d.Backups); err != nil {
			fail("backup settings", err)
		} else {
			m.logf("dashboard: backup settings saved (%s)", d.Backups.Freq)
		}
	}
	if len(d.Nodes) > 0 {
		if m.Server.Nodes == nil {
			m.logf("dashboard: paired nodes skipped: only the main node keeps them")
		} else if n, err := m.Server.Nodes.Import(d.Nodes); err != nil {
			fail("paired nodes", err)
		} else {
			m.logf("dashboard: %d of %d paired node(s) added; the others were already here", n, len(d.Nodes))
		}
	}
	if len(d.Tokens) > 0 {
		if m.Server.Tokens == nil {
			m.logf("dashboard: API tokens skipped: only the main node keeps them")
		} else if n, err := m.Server.Tokens.Import(d.Tokens); err != nil {
			fail("API tokens", err)
		} else {
			m.logf("dashboard: %d of %d API token(s) added; the others were already here", n, len(d.Tokens))
		}
	}
	if d.Port != 0 && d.Port != cfg.Port {
		if dev {
			m.logf("dashboard: port %d skipped: the dev server takes its port from --port", d.Port)
		} else {
			port = d.Port
		}
	}
	if len(failed) > 0 {
		err = errors.New(strings.Join(failed, ", ") + " failed")
	}
	return port, restart, err
}

// importNetwork sets the hostname, time zone and NTP, and the IPv4 settings of the first
// matching connection that differs. That one goes through network.Apply, so it's undone
// unless kept under System → Settings within two minutes; waiting is true then.
func (m *Manager) importNetwork(n *NetworkPart) (waiting bool, err error) {
	var failed []string
	fail := func(what string, err error) {
		m.logf("network: %s: %v", what, err)
		failed = append(failed, what)
	}
	cur := system.GetSettings()
	if n.Hostname != "" && n.Hostname != cur.Hostname {
		if err := system.SetHostname(n.Hostname); err != nil {
			fail("hostname", err)
		} else {
			m.logf("network: hostname set to %s", n.Hostname)
		}
	}
	if n.Timezone != "" && n.Timezone != cur.Timezone {
		if err := system.SetTimezone(n.Timezone); err != nil {
			fail("time zone", err)
		} else {
			m.logf("network: time zone set to %s", n.Timezone)
		}
	}
	if n.NTP != cur.NTP {
		if err := system.SetNTP(n.NTP); err != nil {
			fail("NTP", err)
		} else {
			m.logf("network: NTP turned %s", map[bool]string{true: "on", false: "off"}[n.NTP])
		}
	}
	conns, lerr := network.List()
	if lerr != nil && len(n.Connections) > 0 {
		fail("IPv4", lerr)
	}
	for _, want := range n.Connections {
		i := slices.IndexFunc(conns, func(c network.Conn) bool { return c.Name == want.Name })
		if i < 0 {
			i = slices.IndexFunc(conns, func(c network.Conn) bool { return c.Device == want.Device })
		}
		if i < 0 {
			m.logf("network: no connection named %q or on %s here; skipped", want.Name, want.Device)
			continue
		}
		c := conns[i]
		if c.Method == want.Method && slices.Equal(c.Addresses, want.Addresses) && c.Gateway == want.Gateway &&
			slices.Equal(c.DNS, want.DNS) && c.IgnoreAutoDNS == want.IgnoreAutoDNS {
			m.logf("network: %s already has these IPv4 settings", c.Name)
			continue
		}
		if waiting {
			m.logf("network: %s skipped: one change at a time. Keep the first, then import again", c.Name)
			continue
		}
		next := network.Conn{UUID: c.UUID, Name: c.Name, Device: c.Device, Method: want.Method,
			Addresses: slices.Clone(want.Addresses), Gateway: want.Gateway, DNS: slices.Clone(want.DNS), IgnoreAutoDNS: want.IgnoreAutoDNS}
		if _, err := network.Apply(next); err != nil {
			fail("IPv4 of "+c.Name, err)
			continue
		}
		waiting = true
		m.logf("network: %s set to %s; it's undone in 2 minutes unless kept under System → Settings", c.Name, describeConn(next))
	}
	if len(failed) > 0 {
		err = errors.New(strings.Join(failed, ", ") + " failed")
	}
	return waiting, err
}

func describeConn(c network.Conn) string {
	if c.Method == "manual" {
		return "static " + strings.Join(c.Addresses, ", ")
	}
	return "DHCP"
}
