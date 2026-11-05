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

func TestParseProgress(t *testing.T) {
	log := `Switching to testing (Fedora 45, may break):
[3/9] Fetching ostree chunk aaa (1 MB)...done
Switched to testing. It takes effect after a reboot.
Switching to stable (Fedora 45, production):
  ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab:45
Pulling manifest: ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab:45`
	if p := ParseProgress(log); p.Channel != "stable" || p.About != "Fedora 45, production" || p.Step != "starting" || p.Total != 0 {
		t.Fatalf("%+v", p)
	}
	log += "\n[1/73] Fetching ostree chunk bbb (5.7 MB)...done\n[41/73] Fetching ostree chunk ccc (27.8 MB)...done"
	if p := ParseProgress(log); p.Step != "downloading" || p.Done != 41 || p.Total != 73 {
		t.Fatalf("%+v", p)
	}
	if p := ParseProgress(log + "\nStaging deployment..."); p.Step != "staging" || p.Done != 41 {
		t.Fatalf("%+v", p)
	}
}
