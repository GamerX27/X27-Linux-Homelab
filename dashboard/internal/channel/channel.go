// Package channel shows and switches the release channel through the `channel` command, so
// the dashboard and the CLI never disagree. A switch is an rpm-ostree rebase that downloads a
// whole image, so it runs as its own transient systemd unit: it survives a dashboard restart
// and doesn't hold up the request. Like an update, it applies after a reboot.
package channel

import (
	"encoding/json"
	"errors"
	"os"
	"regexp"
	"strconv"
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
	Available     bool   `json:"available"`
	Current       string `json:"current"` // "" when the image isn't one of ours
	Booted        string `json:"booted"`
	Staged        string `json:"staged"`
	StagedChannel string `json:"stagedChannel"`
	VM            bool   `json:"vm"`
	BootedTag     string `json:"bootedTag"`
	// On its channel's image but a tag nothing builds any more (stable on :44 after stable
	// moved to :45): no more updates. Switching to Current moves it to the channel's tag.
	Outdated    bool      `json:"outdated"`
	BootedAbout string    `json:"bootedAbout"` // what the old tag runs, e.g. "Fedora 44"
	Channels    []Channel `json:"channels"`
	Switching   bool      `json:"switching"`          // a switch is downloading right now
	Progress    *Progress `json:"progress,omitempty"` // how far that switch is, while Switching
	Error       string    `json:"error,omitempty"`
}

// Progress of a running switch, read from its log: rpm-ostree prints "[12/73] Fetching
// ostree chunk …" per layer, then "Staging deployment…".
type Progress struct {
	Channel string `json:"channel"` // the channel it switches to
	About   string `json:"about"`   // e.g. "Fedora 45, production"
	Step    string `json:"step"`    // "starting", "downloading" or "staging"
	Done    int    `json:"done"`    // layers fetched
	Total   int    `json:"total"`
}

var (
	switchLine = regexp.MustCompile(`Switching to (\S+) \(([^)]*)\)`)
	chunkLine  = regexp.MustCompile(`\[(\d+)/(\d+)\] Fetching`)
)

// ParseProgress reads the latest switch in log (a unit's journal can hold earlier ones).
func ParseProgress(log string) *Progress {
	if i := strings.LastIndex(log, "Switching to "); i >= 0 {
		log = log[i:]
	}
	p := &Progress{Step: "starting"}
	if m := switchLine.FindStringSubmatch(log); m != nil {
		p.Channel, p.About = m[1], m[2]
	}
	if all := chunkLine.FindAllStringSubmatch(log, -1); len(all) > 0 {
		last := all[len(all)-1]
		p.Done, _ = strconv.Atoi(last[1])
		p.Total, _ = strconv.Atoi(last[2])
		p.Step = "downloading"
	}
	if strings.Contains(log, "Staging deployment") {
		p.Step = "staging"
	}
	return p
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
	if s.Switching {
		s.Progress = ParseProgress(Log())
	}
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

// The fake node starts on stable's old Fedora 44 tag, so dev mode shows the end-of-life notice.
var fake = struct {
	sync.Mutex
	current, target string
	outdated        bool
	started         time.Time
}{current: "stable", outdated: true}

func fakeChannels() []Channel {
	return []Channel{
		{Name: "stable", About: "Fedora 45, production", Image: "ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab:45"},
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
			if fake.outdated {
				s.Booted = strings.TrimSuffix(c.Image, ":45") + ":44"
				s.Outdated, s.BootedTag, s.BootedAbout = true, "44", "Fedora 44"
			}
		}
		if c.Name == fake.target {
			s.StagedChannel = fake.target
			s.Staged = c.Image
		}
	}
	s.Switching = fake.target != "" && time.Since(fake.started) < fakeSwitchTime
	if s.Switching {
		s.Staged, s.StagedChannel = "", ""
		s.Progress = ParseProgress(fakeLogLocked())
	}
	return s
}

func fakeSwitch(name string) error {
	fake.Lock()
	defer fake.Unlock()
	for _, c := range fakeChannels() {
		if c.Name == name {
			if name == fake.current && !fake.outdated {
				fake.target = ""
				return nil
			}
			fake.target, fake.started = name, time.Now()
			return nil
		}
	}
	return errors.New("unknown channel: " + name)
}

const fakeSwitchTime = 40 * time.Second

func fakeLog() string {
	fake.Lock()
	defer fake.Unlock()
	return fakeLogLocked()
}

// fakeLogLocked fetches 20 "layers" over most of fakeSwitchTime, then stages.
func fakeLogLocked() string {
	if fake.target == "" {
		return ""
	}
	about := fake.target
	for _, c := range fakeChannels() {
		if c.Name == fake.target {
			about = c.About
		}
	}
	lines := []string{"Switching to " + fake.target + " (" + about + "):", "Pulling manifest…"}
	el := time.Since(fake.started)
	n := min(20, int(el*20/(fakeSwitchTime-5*time.Second)))
	for i := 1; i <= n; i++ {
		lines = append(lines, "["+strconv.Itoa(i)+"/20] Fetching ostree chunk 0123abcd (5.0 MB)...done")
	}
	if n == 20 {
		lines = append(lines, "Staging deployment...")
	}
	if el > fakeSwitchTime {
		lines = append(lines, "Staging deployment... done", "", "Switched to "+fake.target+". It takes effect after a reboot.")
	}
	return strings.Join(lines, "\n")
}
