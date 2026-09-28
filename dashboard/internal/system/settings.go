package system

import (
	"errors"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

type Settings struct {
	Hostname string `json:"hostname"`
	Timezone string `json:"timezone"`
	NTP      bool   `json:"ntp"`
	NTPSync  bool   `json:"ntpSynced"`
	Time     string `json:"time"`
}

func timedate(prop string) string {
	out, _ := run.Cmd(5*time.Second, "timedatectl", "show", "-p", prop, "--value")
	return out
}

func GetSettings() Settings {
	s := Settings{Time: time.Now().Format(time.RFC3339)}
	s.Hostname = GetInfo().Hostname
	s.Timezone = timedate("Timezone")
	s.NTP = timedate("NTP") == "yes"
	s.NTPSync = timedate("NTPSynchronized") == "yes"
	return s
}

func Timezones() []string {
	out, err := run.Cmd(10*time.Second, "timedatectl", "list-timezones")
	if err != nil {
		return nil
	}
	return strings.Fields(out)
}

var hostnameRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?$`)

func SetHostname(name string) error {
	if !hostnameRe.MatchString(name) {
		return errors.New("a hostname is 1-63 letters, digits and dashes, not starting or ending with a dash")
	}
	_, err := run.Cmd(10*time.Second, "hostnamectl", "hostname", name)
	return err
}

func SetTimezone(tz string) error {
	if !slices.Contains(Timezones(), tz) {
		return errors.New("unknown time zone")
	}
	_, err := run.Cmd(10*time.Second, "timedatectl", "set-timezone", tz)
	return err
}

func SetNTP(on bool) error {
	v := "false"
	if on {
		v = "true"
	}
	_, err := run.Cmd(10*time.Second, "timedatectl", "set-ntp", v)
	return err
}

// Power runs reboot or poweroff without waiting, so the HTTP reply still goes out.
func Power(action string) error {
	if action != "reboot" && action != "poweroff" {
		return errors.New("unknown power action")
	}
	go func() {
		time.Sleep(time.Second)
		run.Cmd(30*time.Second, "systemctl", action)
	}()
	return nil
}
