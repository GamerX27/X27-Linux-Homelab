// Package netbird drives the NetBird client that ships (off) with the image: its status,
// connecting with a setup key or a browser login, and disconnecting. The daemon itself is
// netbird.service, switched on and off like the other services.
package netbird

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

const (
	netbirdCmd = "/usr/bin/netbird"
	Unit       = "netbird.service"
)

type Peer struct {
	FQDN       string `json:"fqdn"`
	IP         string `json:"ip"`
	Status     string `json:"status"`
	Connection string `json:"connection"` // P2P or Relayed
	LatencyMs  int64  `json:"latencyMs"`
	Received   int64  `json:"received"`
	Sent       int64  `json:"sent"`
}

type Status struct {
	Installed  bool   `json:"installed"`
	Enabled    bool   `json:"enabled"` // netbird.service starts at boot
	Running    bool   `json:"running"` // the daemon runs
	Connected  bool   `json:"connected"`
	State      string `json:"state"` // connected, needs-login, connecting, stopped
	Message    string `json:"message,omitempty"`
	Management string `json:"management"`
	MgmtOK     bool   `json:"managementConnected"`
	SignalOK   bool   `json:"signalConnected"`
	IP         string `json:"ip"`
	FQDN       string `json:"fqdn"`
	Version    string `json:"version"`
	PeersTotal int    `json:"peersTotal"`
	PeersUp    int    `json:"peersConnected"`
	Peers      []Peer `json:"peers"`
	LoginURL   string `json:"loginUrl,omitempty"` // a browser login waiting to be finished
}

type statusJSON struct {
	Peers struct {
		Total     int `json:"total"`
		Connected int `json:"connected"`
		Details   []struct {
			FQDN             string `json:"fqdn"`
			IP               string `json:"netbirdIp"`
			Status           string `json:"status"`
			ConnectionType   string `json:"connectionType"`
			Latency          int64  `json:"latency"` // nanoseconds
			TransferReceived int64  `json:"transferReceived"`
			TransferSent     int64  `json:"transferSent"`
		} `json:"details"`
	} `json:"peers"`
	DaemonVersion string `json:"daemonVersion"`
	Management    struct {
		URL       string `json:"url"`
		Connected bool   `json:"connected"`
	} `json:"management"`
	Signal struct {
		Connected bool `json:"connected"`
	} `json:"signal"`
	IP   string `json:"netbirdIp"`
	FQDN string `json:"fqdn"`
}

// Parse reads `netbird status --json`.
func Parse(b []byte) (Status, error) {
	var j statusJSON
	if err := json.Unmarshal(b, &j); err != nil {
		return Status{}, err
	}
	s := Status{Installed: true, Running: true, Management: j.Management.URL, MgmtOK: j.Management.Connected,
		SignalOK: j.Signal.Connected, IP: j.IP, FQDN: j.FQDN, Version: j.DaemonVersion,
		PeersTotal: j.Peers.Total, PeersUp: j.Peers.Connected, Peers: []Peer{}}
	s.Connected = s.MgmtOK && s.IP != ""
	s.State = "connecting"
	if s.Connected {
		s.State = "connected"
	}
	for _, p := range j.Peers.Details {
		s.Peers = append(s.Peers, Peer{FQDN: p.FQDN, IP: p.IP, Status: p.Status, Connection: p.ConnectionType,
			LatencyMs: p.Latency / int64(time.Millisecond), Received: p.TransferReceived, Sent: p.TransferSent})
	}
	return s, nil
}

func unitState() (enabled, active bool) {
	out, _ := run.Cmd(5*time.Second, "systemctl", "show", Unit, "-p", "UnitFileState", "-p", "ActiveState")
	return strings.Contains(out, "UnitFileState=enabled"), strings.Contains(out, "ActiveState=active")
}

func Get() Status {
	if _, err := os.Stat(netbirdCmd); err != nil {
		return Status{State: "stopped", Message: "NetBird isn't installed on this node."}
	}
	enabled, active := unitState()
	if !active {
		return Status{Installed: true, Enabled: enabled, State: "stopped"}
	}
	out, err := run.Cmd(15*time.Second, netbirdCmd, "status", "--json")
	s, perr := Parse([]byte(out))
	if err != nil || perr != nil {
		s = Status{Installed: true, Running: true, State: "connecting", Message: out}
		if strings.Contains(out, "NeedsLogin") || strings.Contains(out, "LoginFailed") {
			s.State, s.Message = "needs-login", ""
		}
	}
	s.Enabled = enabled
	login.Lock()
	if login.cmd != nil && !s.Connected {
		s.LoginURL = login.url
	}
	login.Unlock()
	return s
}

var (
	urlRe = regexp.MustCompile(`^https?://[^\s]+$`)
	keyRe = regexp.MustCompile(`^[A-Za-z0-9-]{8,128}$`)
)

func upArgs(mgmtURL string) ([]string, error) {
	args := []string{"up"}
	if mgmtURL != "" {
		if !urlRe.MatchString(mgmtURL) {
			return nil, errors.New("the management URL must start with https:// (or http://)")
		}
		args = append(args, "--management-url", mgmtURL)
	}
	return args, nil
}

func needDaemon() error {
	if _, err := os.Stat(netbirdCmd); err != nil {
		return errors.New("NetBird isn't installed on this node")
	}
	if _, active := unitState(); !active {
		return errors.New("turn the NetBird service on first")
	}
	return nil
}

// Up connects with a setup key. The key goes through a root-only file, not the command line.
func Up(setupKey, mgmtURL string) (string, error) {
	if err := needDaemon(); err != nil {
		return "", err
	}
	setupKey = strings.TrimSpace(setupKey)
	if !keyRe.MatchString(setupKey) {
		return "", errors.New("that doesn't look like a NetBird setup key")
	}
	args, err := upArgs(mgmtURL)
	if err != nil {
		return "", err
	}
	f, err := os.CreateTemp("", "netbird-key-*")
	if err != nil {
		return "", err
	}
	defer os.Remove(f.Name())
	f.WriteString(setupKey)
	f.Close()
	return run.Cmd(2*time.Minute, netbirdCmd, append(args, "--setup-key-file", f.Name())...)
}

// A browser login: `netbird up --no-browser` prints a URL and waits until someone logs in.
var login struct {
	sync.Mutex
	cmd *exec.Cmd
	url string
}

var loginURLRe = regexp.MustCompile(`https?://\S+`)

// Login starts a browser login and returns its URL. The command keeps waiting in the
// background (up to 15 minutes) for the login to finish; Get shows when it has.
func Login(mgmtURL string) (string, error) {
	if err := needDaemon(); err != nil {
		return "", err
	}
	args, err := upArgs(mgmtURL)
	if err != nil {
		return "", err
	}
	login.Lock()
	defer login.Unlock()
	if login.cmd != nil && login.url != "" {
		return login.url, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	cmd := exec.CommandContext(ctx, netbirdCmd, append(args, "--no-browser")...)
	out, err := cmd.StdoutPipe()
	if err != nil {
		cancel()
		return "", err
	}
	cmd.Stderr = cmd.Stdout
	if err := cmd.Start(); err != nil {
		cancel()
		return "", err
	}
	login.cmd, login.url = cmd, ""
	found := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(out)
		var text []string
		for sc.Scan() {
			text = append(text, sc.Text())
			if u := loginURLRe.FindString(sc.Text()); u != "" {
				select {
				case found <- u:
				default:
				}
			}
		}
		select {
		case found <- "\x00" + strings.Join(text, "\n"):
		default:
		}
		cmd.Wait()
		cancel()
		login.Lock()
		if login.cmd == cmd {
			login.cmd, login.url = nil, ""
		}
		login.Unlock()
	}()
	login.Unlock()
	var res string
	select {
	case res = <-found:
	case <-time.After(30 * time.Second):
		res = "\x00netbird didn't print a login link"
	}
	login.Lock()
	if rest, failed := strings.CutPrefix(res, "\x00"); failed {
		if strings.TrimSpace(rest) == "" {
			// It exited without a link: it was logged in already and just connected.
			return "", nil
		}
		return "", errors.New(strings.TrimSpace(rest))
	}
	if login.cmd == cmd {
		login.url = res
	}
	return res, nil
}

// Down disconnects. The daemon keeps running, so Up or Login reconnects.
func Down() (string, error) {
	if err := needDaemon(); err != nil {
		return "", err
	}
	login.Lock()
	if login.cmd != nil && login.cmd.Process != nil {
		login.cmd.Process.Kill()
	}
	login.Unlock()
	return run.Cmd(time.Minute, netbirdCmd, "down")
}
