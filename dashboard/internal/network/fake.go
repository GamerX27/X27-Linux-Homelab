package network

import (
	"errors"
	"slices"
	"time"
)

// Fake is only for `dashboard serve --dev` with DASHBOARD_FAKE_NETWORK=1: one wired
// connection kept in memory, so the Network card can be tried on a machine that isn't a
// node. A change reverts itself after RevertAfter like the real one; nothing is touched.
var Fake bool

var fake = struct {
	conn, old Conn
	pending   *Pending
}{conn: Conn{
	UUID: "0b7e2a52-5d1c-4c1e-9a7d-2f6c1e3b8a10", Name: "Wired connection 1", Device: "eth0", Type: "802-3-ethernet",
	Method: "auto", Addresses: []string{}, DNS: []string{},
	Current: Live{Addresses: []string{"192.168.1.50/24"}, Gateway: "192.168.1.1", DNS: []string{"192.168.1.1"}},
}}

// fakeLive is what a connection would get: its static settings, or the fake DHCP lease.
func fakeLive(c Conn) Live {
	l := Live{Addresses: []string{"192.168.1.50/24"}, Gateway: "192.168.1.1", DNS: []string{"192.168.1.1"}}
	if c.Method == "manual" {
		l.Addresses, l.Gateway = slices.Clone(c.Addresses), c.Gateway
		l.DNS = []string{}
	}
	if len(c.DNS) > 0 {
		l.DNS = slices.Clone(c.DNS)
	}
	return l
}

func fakeList() []Conn {
	mu.Lock()
	defer mu.Unlock()
	fakeExpire()
	return []Conn{fake.conn}
}

func fakeExpire() {
	if fake.pending != nil && time.Now().Unix() >= fake.pending.Deadline {
		fake.conn, fake.pending = fake.old, nil
	}
}

func fakePending() *Pending {
	fakeExpire()
	return fake.pending
}

func fakeApply(c Conn) (Pending, error) {
	if c.UUID != fake.conn.UUID {
		return Pending{}, errors.New("there's no active connection with that ID")
	}
	c.Name, c.Device, c.Type = fake.conn.Name, fake.conn.Device, fake.conn.Type
	c.Current = fakeLive(c)
	fake.old, fake.conn = fake.conn, c
	fake.pending = &Pending{Deadline: time.Now().Add(RevertAfter).Unix(), UUID: c.UUID, Name: c.Name}
	return *fake.pending, nil
}

func fakeConfirm(keep bool) error {
	if !keep {
		fake.conn = fake.old
	}
	fake.pending = nil
	return nil
}
