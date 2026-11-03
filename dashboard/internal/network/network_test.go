package network

import (
	"slices"
	"testing"
)

func TestSplitTerse(t *testing.T) {
	f := splitTerse(`0b7e:Wired\: office:802-3-ethernet:eth0`)
	if !slices.Equal(f, []string{"0b7e", "Wired: office", "802-3-ethernet", "eth0"}) {
		t.Fatalf("%q", f)
	}
}

func TestParseDetail(t *testing.T) {
	var c Conn
	parseDetail(&c, `ipv4.method:manual
ipv4.dns:1.1.1.1,9.9.9.9
ipv4.dns-search:
ipv4.addresses:192.168.1.10/24
ipv4.gateway:192.168.1.1
ipv4.ignore-auto-dns:no
IP4.ADDRESS[1]:192.168.1.10/24
IP4.ADDRESS[2]:10.0.0.5/8
IP4.GATEWAY:192.168.1.1
IP4.DNS[1]:1.1.1.1`)
	if c.Method != "manual" || !slices.Equal(c.Addresses, []string{"192.168.1.10/24"}) || c.Gateway != "192.168.1.1" ||
		!slices.Equal(c.DNS, []string{"1.1.1.1", "9.9.9.9"}) || c.IgnoreAutoDNS ||
		!slices.Equal(c.Current.Addresses, []string{"192.168.1.10/24", "10.0.0.5/8"}) || !slices.Equal(c.Current.DNS, []string{"1.1.1.1"}) {
		t.Fatalf("%+v", c)
	}

	var d Conn
	parseDetail(&d, "ipv4.method:auto\nipv4.addresses:\nipv4.gateway:--\nipv4.dns:\n")
	if d.Method != "auto" || len(d.Addresses) != 0 || d.Gateway != "" || len(d.DNS) != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestValidate(t *testing.T) {
	bad := []Conn{
		{Method: "static"},
		{Method: "manual"},
		{Method: "manual", Addresses: []string{"192.168.1.10"}},
		{Method: "manual", Addresses: []string{"fd00::1/64"}},
		{Method: "manual", Addresses: []string{"192.168.1.10/24"}, Gateway: "10.0.0.1"},
		{Method: "manual", Addresses: []string{"192.168.1.10/24"}, Gateway: "192.168.1.10"},
		{Method: "auto", DNS: []string{"dns.example"}},
		{Method: "auto", DNS: []string{"1.1.1.1", "1.0.0.1", "9.9.9.9", "8.8.8.8"}},
	}
	for _, c := range bad {
		if err := Validate(&c); err == nil {
			t.Errorf("%+v passed", c)
		}
	}

	c := Conn{Method: "manual", Addresses: []string{" 192.168.1.10/24"}, Gateway: "192.168.1.1", DNS: []string{"1.1.1.1"}}
	if err := Validate(&c); err != nil || c.Addresses[0] != "192.168.1.10/24" || c.IgnoreAutoDNS {
		t.Fatal(c, err)
	}
	d := Conn{Method: "auto", Addresses: []string{"192.168.1.10/24"}, Gateway: "192.168.1.1", DNS: []string{"1.1.1.1"}}
	if err := Validate(&d); err != nil || len(d.Addresses) != 0 || d.Gateway != "" || !d.IgnoreAutoDNS {
		t.Fatal(d, err)
	}
}

func TestModifyArgs(t *testing.T) {
	a := modifyArgs(Conn{UUID: "u", Method: "auto", DNS: []string{"1.1.1.1", "9.9.9.9"}, IgnoreAutoDNS: true})
	want := []string{"connection", "modify", "u", "ipv4.method", "auto", "ipv4.addresses", "", "ipv4.gateway", "",
		"ipv4.dns", "1.1.1.1,9.9.9.9", "ipv4.ignore-auto-dns", "yes"}
	if !slices.Equal(a, want) {
		t.Fatalf("%q", a)
	}
}
