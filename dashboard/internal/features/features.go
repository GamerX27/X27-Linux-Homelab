// Package features manages what the image itself ships: automatic updates and their
// Gotify messages go through the `autoupdate` command, so the dashboard and the CLI never
// disagree; the rest are systemd units that can be switched on and off.
package features

import (
	"errors"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

const (
	autoupdateCmd = "/usr/bin/autoupdate"
	timerFile     = "/etc/systemd/system/autoupdate.timer"
	gotifyConf    = "/etc/autoupdate.conf"
)

type Autoupdate struct {
	Available  bool   `json:"available"`
	Enabled    bool   `json:"enabled"`
	Schedule   string `json:"schedule"`   // "weekly on Sun at 03:30 (3:30 AM)"
	Calendar   string `json:"calendar"`   // OnCalendar= value
	NextRun    int64  `json:"nextRun"`    // unix seconds
	LastRun    int64  `json:"lastRun"`    // unix seconds
	LastResult string `json:"lastResult"` // success, exit-code, …
	Gotify     bool   `json:"gotify"`
	GotifyURL  string `json:"gotifyUrl"`
}

type Service struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Unit        string `json:"unit"`
	Enabled     bool   `json:"enabled"`
	Active      string `json:"active"`
	Warning     string `json:"warning,omitempty"`
}

type All struct {
	Autoupdate Autoupdate `json:"autoupdate"`
	Services   []Service  `json:"services"`
}

// services is the registry of units the UI may switch. A unit that isn't installed
// (smartd on the VM image, the guest agent on bare metal) is left out.
var services = []Service{
	{ID: "ssh", Name: "SSH server", Unit: "sshd.service", Description: "Remote logins over SSH.",
		Warning: "Turning SSH off leaves this dashboard as the only way in."},
	{ID: "smartd", Name: "Disk health monitoring", Unit: "smartd.service", Description: "smartd watches SMART data and logs failing disks."},
	{ID: "docker-group", Name: "Docker group for users", Unit: "docker-group.service", Description: "Adds regular users to the docker group at boot."},
	{ID: "fish-default-shell", Name: "fish as default shell", Unit: "fish-default-shell.service", Description: "Makes fish the login shell for regular users at boot."},
	{ID: "qemu-guest-agent", Name: "QEMU guest agent", Unit: "qemu-guest-agent.service", Description: "Lets the hypervisor see IPs and shut the VM down cleanly."},
	{ID: "docker", Name: "Docker", Unit: "docker.service", Description: "The Docker engine; every service on this host runs in it.",
		Warning: "Turning Docker off stops every container."},
}

func show(unit string, props ...string) map[string]string {
	args := []string{"show", unit, "--timestamp=unix"}
	for _, p := range props {
		args = append(args, "-p", p)
	}
	out, _ := run.Cmd(5*time.Second, "systemctl", args...)
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			m[k] = v
		}
	}
	return m
}

func unixTS(v string) int64 {
	var n int64
	if strings.HasPrefix(v, "@") {
		for _, r := range v[1:] {
			if r < '0' || r > '9' {
				break
			}
			n = n*10 + int64(r-'0')
		}
	}
	return n
}

func unitExists(unit string) bool {
	return show(unit, "LoadState")["LoadState"] == "loaded"
}

func GetAutoupdate() Autoupdate {
	a := Autoupdate{}
	if _, err := os.Stat(autoupdateCmd); err != nil {
		return a
	}
	a.Available = true
	if b, err := os.ReadFile(timerFile); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "# Schedule: "); ok {
				a.Schedule = v
			}
			if v, ok := strings.CutPrefix(line, "OnCalendar="); ok {
				a.Calendar = v
			}
		}
		t := show("autoupdate.timer", "UnitFileState", "NextElapseUSecRealtime")
		a.Enabled = t["UnitFileState"] == "enabled"
		a.NextRun = unixTS(t["NextElapseUSecRealtime"])
	}
	s := show("autoupdate.service", "ExecMainExitTimestamp", "Result")
	a.LastRun = unixTS(s["ExecMainExitTimestamp"])
	if a.LastRun > 0 {
		a.LastResult = s["Result"]
	}
	if b, err := os.ReadFile(gotifyConf); err == nil {
		a.Gotify = true
		for _, line := range strings.Split(string(b), "\n") {
			if v, ok := strings.CutPrefix(line, "GOTIFY_URL="); ok {
				a.GotifyURL = v
			}
		}
	}
	return a
}

func Get() All {
	all := All{Autoupdate: GetAutoupdate()}
	for _, s := range services {
		st := show(s.Unit, "LoadState", "UnitFileState", "ActiveState")
		if st["LoadState"] != "loaded" {
			continue
		}
		s.Enabled = st["UnitFileState"] == "enabled"
		s.Active = st["ActiveState"]
		all.Services = append(all.Services, s)
	}
	return all
}

var (
	timeRe    = regexp.MustCompile(`^([01]?[0-9]|2[0-3]):[0-5][0-9]$`)
	weekdayRe = regexp.MustCompile(`^(mon|tue|wed|thu|fri|sat|sun)$`)
	dayRe     = regexp.MustCompile(`^([1-9]|[12][0-9]|3[01])$`)
)

// ScheduleArgs validates a schedule from the UI and turns it into `autoupdate on …` arguments.
func ScheduleArgs(freq, weekday, day, clock string) ([]string, error) {
	if !timeRe.MatchString(clock) {
		return nil, errors.New("time must be HH:MM (24-hour)")
	}
	switch freq {
	case "daily":
		return []string{"on", "daily", clock}, nil
	case "weekly":
		weekday = strings.ToLower(weekday)
		if !weekdayRe.MatchString(weekday) {
			return nil, errors.New("pick a weekday")
		}
		return []string{"on", "weekly", weekday, clock}, nil
	case "monthly":
		if !dayRe.MatchString(day) {
			return nil, errors.New("day of month must be 1-31")
		}
		return []string{"on", "monthly", day, clock}, nil
	}
	return nil, errors.New("choose daily, weekly or monthly")
}

func autoupdate(args ...string) (string, error) {
	return run.Cmd(2*time.Minute, autoupdateCmd, args...)
}

func SetAutoupdate(freq, weekday, day, clock string) (string, error) {
	args, err := ScheduleArgs(freq, weekday, day, clock)
	if err != nil {
		return "", err
	}
	return autoupdate(args...)
}

func AutoupdateOff() (string, error) { return autoupdate("off") }

var urlRe = regexp.MustCompile(`^https?://\S+$`)
var tokenRe = regexp.MustCompile(`^[[:graph:]]+$`)

// SetGotify stores a server; autoupdate sends a test message and saves only if it arrives.
func SetGotify(url, token string) (string, error) {
	if !urlRe.MatchString(url) {
		return "", errors.New("URL must start with http:// or https://")
	}
	if !tokenRe.MatchString(token) {
		return "", errors.New("enter the Gotify app token")
	}
	// Without a token argument autoupdate reads it from stdin, keeping it out of ps.
	out, err := run.CmdInput(2*time.Minute, token+"\n", autoupdateCmd, "gotify", url)
	if err != nil {
		return "", errors.New(strings.TrimPrefix(err.Error(), "Gotify app token: "))
	}
	return strings.TrimPrefix(out, "Gotify app token: "), nil
}

func GotifyTest() (string, error) { return autoupdate("gotify", "test") }
func GotifyOff() (string, error)  { return autoupdate("gotify", "off") }

// SetService enables and starts, or disables and stops, a unit from the registry.
func SetService(id string, on bool) error {
	for _, s := range services {
		if s.ID != id {
			continue
		}
		if !unitExists(s.Unit) {
			return errors.New(s.Unit + " isn't installed")
		}
		verb := "disable"
		if on {
			verb = "enable"
		}
		_, err := run.Cmd(2*time.Minute, "systemctl", verb, "--now", s.Unit)
		return err
	}
	return errors.New("unknown feature")
}
