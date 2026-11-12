package vip

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
)

func TestRender(t *testing.T) {
	c := Config{Enabled: true, Address: "192.168.1.50", Prefix: 24, Role: RoleMain, Priority: 150,
		Peers: []string{"192.168.1.11", "192.168.1.12"}, RouterID: 42, Password: "abcd1234", Port: 9090}
	out, err := Render(c, Iface{Name: "enp1s0", IP: "192.168.1.10"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"interface enp1s0", "virtual_router_id 42", "priority 150", "preempt_delay 30",
		"unicast_src_ip 192.168.1.10", "        192.168.1.11\n        192.168.1.12\n    }", "auth_pass abcd1234",
		"192.168.1.50/24 dev enp1s0", "dashboard-check 9090", `notify_master "/usr/libexec/keepalived/dashboard-notify main master"`,
		`notify_fault "/usr/libexec/keepalived/dashboard-notify main fault"`} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "nopreempt") {
		t.Error("failback is on")
	}
	c.Role, c.Priority, c.Nopreempt = RoleStandby, 130, true
	out, _ = Render(c, Iface{Name: "enp1s0", IP: "192.168.1.12"})
	if !strings.Contains(out, "priority 130") || !strings.Contains(out, "nopreempt") || strings.Contains(out, "preempt_delay") ||
		!strings.Contains(out, "dashboard-notify standby backup") {
		t.Errorf("third member:\n%s", out)
	}
}

func TestUpgradeOldPair(t *testing.T) {
	old := Config{Enabled: true, Role: RoleStandby, Peer: "10.0.0.2", Address: "10.0.0.5", Prefix: 24, RouterID: 3, Password: "x"}
	c := upgrade(old)
	if len(c.Peers) != 1 || c.Peers[0] != "10.0.0.2" || c.Priority != 100 {
		t.Fatalf("%+v", c)
	}
	if err := Validate(c); err != nil {
		t.Fatal(err)
	}
	old.Role = RoleMain
	if c := upgrade(old); c.Priority != 150 {
		t.Fatalf("%+v", c)
	}
}

func TestValidate(t *testing.T) {
	ok := Config{Enabled: true, Address: "10.0.0.5", Prefix: 24, Role: RoleMain, Priority: 150, Peers: []string{"10.0.0.2"}, RouterID: 1, Password: "x"}
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []func(c *Config){
		func(c *Config) { c.Address = "fe80::1" },
		func(c *Config) { c.Prefix = 31 },
		func(c *Config) { c.Peers = nil },
		func(c *Config) { c.Peers = []string{"10.0.0.2", "nas"} },
		func(c *Config) { c.Priority = 255 },
		func(c *Config) { c.Password = "123456789" },
	} {
		c := ok
		bad(&c)
		if Validate(c) == nil {
			t.Errorf("accepted %+v", c)
		}
	}
	if Validate(Config{}) != nil {
		t.Error("off needs nothing")
	}
}

func TestRelease(t *testing.T) {
	old := config.StateDir
	config.StateDir = t.TempDir()
	Fake = true
	defer func() { config.StateDir = old; cached = nil; Fake = false }()
	cached = nil
	if err := save(Config{Enabled: true, Role: RoleStandby, Address: "10.0.0.5", Prefix: 24}); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(config.StateDir, "vip-fake-holds"), nil, 0o600)
	if !Holds() {
		t.Fatal("should hold")
	}
	if err := Release(true); err != nil || !Releasing() || Holds() {
		t.Fatal("release didn't let go")
	}
	if err := Release(false); err != nil || Releasing() || !Holds() {
		t.Fatal("unrelease")
	}
	Release(false) // twice is fine
}

func TestReceiveBundleOnlyOnMembers(t *testing.T) {
	old := config.StateDir
	config.StateDir = t.TempDir()
	defer func() { config.StateDir = old; cached = nil }()
	cached = nil
	b := Bundle{MainName: "tower", Cert: []byte("cert"), Key: []byte("key"), Tokens: []byte(`[]`), At: 5}
	if ReceiveBundle(b) == nil {
		t.Fatal("a node that isn't a member took it")
	}
	Fake = true
	defer func() { Fake = false }()
	if err := save(Config{Enabled: true, Role: RoleStandby, Address: "10.0.0.5", Prefix: 24}); err != nil {
		t.Fatal(err)
	}
	if err := ReceiveBundle(b); err != nil {
		t.Fatal(err)
	}
	if k, _ := os.ReadFile(filepath.Join(StandbyTLSDir(), "key.pem")); string(k) != "key" {
		t.Fatal("key not stored")
	}
	if c := Load(); c.MainName != "tower" || c.SyncedAt != 5 {
		t.Fatalf("%+v", c)
	}
	if !IsVIP("10.0.0.5") || IsVIP("10.0.0.6") {
		t.Fatal("IsVIP")
	}
}
