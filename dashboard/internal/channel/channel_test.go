package channel

import "testing"

func TestParse(t *testing.T) {
	s, err := Parse([]byte(`{"current":"testing","booted":"ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab-vm-testing:45",
		"staged":"","stagedChannel":"","vm":true,
		"channels":[{"name":"stable","about":"Fedora 44, production","image":"ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab-vm:44"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Available || s.Current != "testing" || !s.VM || len(s.Channels) != 1 || s.Channels[0].Name != "stable" {
		t.Fatalf("%+v", s)
	}
	if _, err := Parse([]byte("channel: not json")); err == nil {
		t.Fatal("want an error")
	}
}

func TestFakeSwitch(t *testing.T) {
	if err := fakeSwitch("nope"); err == nil {
		t.Fatal("unknown channel accepted")
	}
	if err := fakeSwitch("testing"); err != nil {
		t.Fatal(err)
	}
	if s := fakeGet(); !s.Switching || s.Current != "stable" {
		t.Fatalf("%+v", s)
	}
}
