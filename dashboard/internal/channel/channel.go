// Package channel shows and switches the release channel through the `channel` command, so
// the dashboard and the CLI never disagree. A switch is an rpm-ostree rebase that downloads a
// whole image, so it runs as its own transient systemd unit: it survives a dashboard restart
// and doesn't hold up the request. Like an update, it applies after a reboot.
package channel

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/osupdate"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

const (
	channelCmd = "/usr/bin/channel"
	switchUnit = "x27-channel-switch"
)

type Channel struct {
	Name  string `json:"name"`
	About string `json:"about"`
	Image string `json:"image"`
}

type Status struct {
	Available     bool      `json:"available"`
	Current       string    `json:"current"` // "" when the image isn't one of ours
	Booted        string    `json:"booted"`
	Staged        string    `json:"staged"`
	StagedChannel string    `json:"stagedChannel"`
	VM            bool      `json:"vm"`
	Channels      []Channel `json:"channels"`
	Switching     bool      `json:"switching"` // a switch is downloading right now
	Error         string    `json:"error,omitempty"`
}

// Parse reads `channel --json`.
func Parse(b []byte) (Status, error) {
	var s Status
	if err := json.Unmarshal(b, &s); err != nil {
		return Status{}, err
	}
	s.Available = true
	return s, nil
}

func running() bool {
	out, _ := run.Cmd(5*time.Second, "systemctl", "show", "-p", "ActiveState", "--value", switchUnit+".service")
	return osupdate.AnyActive(out)
}

func Get() Status {
	if osupdate.Fake {
		return fakeGet()
	}
	if _, err := os.Stat(channelCmd); err != nil {
		return Status{Error: "the channel command isn't installed on this node"}
	}
	out, err := run.Cmd(30*time.Second, channelCmd, "--json")
	if err != nil {
		return Status{Error: out}
	}
	s, err := Parse([]byte(out))
	if err != nil {
		return Status{Error: "channel --json: " + err.Error()}
	}
	s.Switching = running()
	return s
}

// Switch starts the rebase onto a channel in the background; follow it with Log.
func Switch(name string) error {
	if osupdate.Fake {
		return fakeSwitch(name)
	}
	s := Get()
	if !s.Available {
		return errors.New(s.Error)
	}
	known := false
	for _, c := range s.Channels {
		known = known || c.Name == name
	}
	if !known {
		return errors.New("unknown channel: " + name)
	}
	if s.Switching {
		return errors.New("a channel switch is already running")
	}
	if j := osupdate.ReadJob(osupdate.BootID(), false); j.Running() && j.Phase != osupdate.PhaseVerifying {
		return errors.New("an OS update is running; switch when it's done")
	}
	// A unit from an earlier switch stays loaded only if it failed; clear it so the name is free.
	run.Cmd(5*time.Second, "systemctl", "reset-failed", switchUnit+".service")
	_, err := run.Cmd(15*time.Second, "systemd-run", "--unit="+switchUnit, "--collect",
		"--description=Switch release channel to "+name, channelCmd, name)
	return err
}

// Log is the output of the latest switch, from the journal.
func Log() string {
	if osupdate.Fake {
		return fakeLog()
	}
	out, _ := run.Cmd(10*time.Second, "journalctl", "-u", switchUnit+".service", "-o", "cat", "-n", "300", "--no-pager")
	return out
}

// ---- Fake (dashboard serve --dev with DASHBOARD_FAKE_OS) --------------------------------

var fake = struct {
	sync.Mutex
	current, target string
	started         time.Time
}{current: "stable"}

func fakeChannels() []Channel {
	return []Channel{
		{Name: "stable", About: "Fedora 44, production", Image: "ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab:44"},
		{Name: "testing", About: "Fedora 45, may break", Image: "ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab-testing:45"},
		{Name: "testing-alma", About: "AlmaLinux 10, may break", Image: "ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab-alma-testing:10"},
	}
}

func fakeGet() Status {
	fake.Lock()
	defer fake.Unlock()
	s := Status{Available: true, Current: fake.current, Channels: fakeChannels()}
	for _, c := range s.Channels {
		if c.Name == fake.current {
			s.Booted = c.Image
		}
		if c.Name == fake.target {
			s.StagedChannel = fake.target
			s.Staged = c.Image
		}
	}
	s.Switching = fake.target != "" && time.Since(fake.started) < 8*time.Second
	if s.Switching {
		s.Staged, s.StagedChannel = "", ""
	}
	return s
}

func fakeSwitch(name string) error {
	fake.Lock()
	defer fake.Unlock()
	for _, c := range fakeChannels() {
		if c.Name == name {
			if name == fake.current {
				fake.target = ""
				return nil
			}
			fake.target, fake.started = name, time.Now()
			return nil
		}
	}
	return errors.New("unknown channel: " + name)
}

func fakeLog() string {
	fake.Lock()
	defer fake.Unlock()
	if fake.target == "" {
		return ""
	}
	lines := []string{"Switching to " + fake.target + ":", "Pulling manifest…"}
	if time.Since(fake.started) > 8*time.Second {
		lines = append(lines, "Staging deployment... done", "", "Switched to "+fake.target+". It takes effect after a reboot.")
	}
	return strings.Join(lines, "\n")
}
