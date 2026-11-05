package channel

import "testing"

func TestParse(t *testing.T) {
	s, err := Parse([]byte(`{"current":"testing","booted":"ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab-vm-testing:45",
		"staged":"","stagedChannel":"","vm":true,
		"channels":[{"name":"stable","about":"Fedora 45, production","image":"ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab-vm:45"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if !s.Available || s.Current != "testing" || !s.VM || len(s.Channels) != 1 || s.Channels[0].Name != "stable" {
		t.Fatalf("%+v", s)
	}
	s, err = Parse([]byte(`{"current":"stable","booted":"ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab:44",
		"bootedTag":"44","outdated":true,"bootedAbout":"Fedora 44","channels":[]}`))
	if err != nil || !s.Outdated || s.BootedTag != "44" || s.BootedAbout != "Fedora 44" {
		t.Fatalf("%+v %v", s, err)
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
	if s := fakeGet(); !s.Switching || s.Current != "stable" || !s.Outdated {
		t.Fatalf("%+v", s)
	}
	// Outdated: switching to the channel it's on is the move to the current tag.
	if err := fakeSwitch("stable"); err != nil || fake.target != "stable" {
		t.Fatalf("%v %q", err, fake.target)
	}
}
