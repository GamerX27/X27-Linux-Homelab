package vip

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
)

func TestRender(t *testing.T) {
	c := Config{Enabled: true, Address: "192.168.1.50", Prefix: 24, Role: RoleMain, Peer: "192.168.1.11", RouterID: 42, Password: "abcd1234", Port: 9090}
	out, err := Render(c, Iface{Name: "enp1s0", IP: "192.168.1.10"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"interface enp1s0", "virtual_router_id 42", "priority 150", "preempt_delay 30",
		"unicast_src_ip 192.168.1.10", "192.168.1.11", "auth_pass abcd1234", "192.168.1.50/24 dev enp1s0", "dashboard-check 9090"} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if strings.Contains(out, "notify_master") {
		t.Error("the main node restarts nothing")
	}
	c.Role = RoleStandby
	out, _ = Render(c, Iface{Name: "enp1s0", IP: "192.168.1.11"})
	if !strings.Contains(out, "priority 100") || !strings.Contains(out, "notify_master") || strings.Contains(out, "preempt_delay") {
		t.Errorf("standby:\n%s", out)
	}
}

func TestValidate(t *testing.T) {
	ok := Config{Enabled: true, Address: "10.0.0.5", Prefix: 24, Role: RoleMain, Peer: "10.0.0.2", RouterID: 1, Password: "x"}
	if err := Validate(ok); err != nil {
		t.Fatal(err)
	}
	for _, bad := range []func(c *Config){
		func(c *Config) { c.Address = "fe80::1" },
		func(c *Config) { c.Prefix = 31 },
		func(c *Config) { c.Peer = "" },
		func(c *Config) { c.Role = "boss" },
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

func TestReceiveBundleOnlyOnStandby(t *testing.T) {
	old := config.StateDir
	config.StateDir = t.TempDir()
	defer func() { config.StateDir = old; cached = nil }()
	cached = nil
	b := Bundle{MainName: "tower", Cert: []byte("cert"), Key: []byte("key"), Tokens: []byte(`[]`), At: 5}
	if ReceiveBundle(b) == nil {
		t.Fatal("a node that isn't the standby took it")
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
