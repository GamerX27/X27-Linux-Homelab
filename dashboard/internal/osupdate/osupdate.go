// Package osupdate shows which image the node runs and whether a newer one is out.
// It only reads through rpm-ostree; installing reuses autoupdate.service (autoupdate-run),
// so an update from the dashboard behaves exactly like a scheduled one: staged, Gotify
// message, reboot.
package osupdate

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

const checkEvery = 6 * time.Hour

type Deployment struct {
	Version   string `json:"version"`
	Timestamp int64  `json:"timestamp"`
	Image     string `json:"image"`
	Digest    string `json:"digest"`
	Booted    bool   `json:"booted"`
	Staged    bool   `json:"staged"`
	Pinned    bool   `json:"pinned"`
}

type Check struct {
	CheckedAt int64  `json:"checkedAt"` // unix seconds, 0 = never
	Checking  bool   `json:"checking"`
	Available bool   `json:"available"`
	Version   string `json:"version"`
	Digest    string `json:"digest"`
	Error     string `json:"error"`
}

type Status struct {
	Supported   bool         `json:"supported"`
	Deployments []Deployment `json:"deployments"`
	Booted      *Deployment  `json:"booted"`
	Staged      *Deployment  `json:"staged"`
	CanRollback bool         `json:"canRollback"`
	Check       Check        `json:"check"`
	Updating    bool         `json:"updating"` // autoupdate.service is running
	Error       string       `json:"error,omitempty"`
}

type Checker struct {
	mu    sync.Mutex
	check Check
}

func Supported() bool {
	_, err := os.Stat("/run/ostree-booted")
	return err == nil
}

// NewChecker checks shortly after start and then every six hours.
func NewChecker() *Checker {
	c := &Checker{}
	if Supported() {
		go func() {
			time.Sleep(30 * time.Second)
			for {
				c.Run()
				time.Sleep(checkEvery)
			}
		}()
	}
	return c
}

type rpmStatus struct {
	Deployments []struct {
		Version   string `json:"version"`
		Timestamp int64  `json:"timestamp"`
		Image     string `json:"container-image-reference"`
		Digest    string `json:"container-image-reference-digest"`
		Checksum  string `json:"checksum"`
		Booted    bool   `json:"booted"`
		Staged    bool   `json:"staged"`
		Pinned    bool   `json:"pinned"`
	} `json:"deployments"`
	CachedUpdate *struct {
		Version string `json:"version"`
		Digest  string `json:"container-image-reference-digest"`
	} `json:"cached-update"`
}

// ParseStatus reads `rpm-ostree status --json`.
func ParseStatus(b []byte) (Status, error) {
	var rs rpmStatus
	if err := json.Unmarshal(b, &rs); err != nil {
		return Status{}, err
	}
	st := Status{Supported: true}
	bootedIdx := -1
	for i, d := range rs.Deployments {
		dep := Deployment{Version: d.Version, Timestamp: d.Timestamp, Digest: d.Digest,
			Booted: d.Booted, Staged: d.Staged, Pinned: d.Pinned}
		dep.Image = d.Image
		if i := strings.Index(dep.Image, "docker://"); i >= 0 {
			dep.Image = dep.Image[i+len("docker://"):]
		}
		if dep.Digest == "" {
			dep.Digest = d.Checksum
		}
		st.Deployments = append(st.Deployments, dep)
		if d.Booted {
			bootedIdx = i
		}
	}
	for i := range st.Deployments {
		switch {
		case st.Deployments[i].Booted:
			st.Booted = &st.Deployments[i]
		case st.Deployments[i].Staged:
			st.Staged = &st.Deployments[i]
		case bootedIdx >= 0 && i > bootedIdx:
			st.CanRollback = true
		}
	}
	return st, nil
}

// ParseCheck reads the text rpm-ostree prints under "AvailableUpdate:".
func ParseCheck(out string) (version, digest string) {
	in := false
	for _, line := range strings.Split(out, "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "AvailableUpdate") {
			in = true
			continue
		}
		if !in {
			continue
		}
		k, v, ok := strings.Cut(t, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		switch strings.TrimSpace(k) {
		case "Version":
			version, _, _ = strings.Cut(v, " (")
		case "Digest":
			digest = v
		}
	}
	return version, digest
}

func (c *Checker) Status() Status {
	if Fake {
		return fakeStatus()
	}
	if !Supported() {
		return Status{Error: "not an rpm-ostree system"}
	}
	out, err := run.Cmd(20*time.Second, "rpm-ostree", "status", "--json")
	st, perr := ParseStatus([]byte(out))
	if err != nil || perr != nil {
		st = Status{Supported: true, Error: "rpm-ostree status failed: " + out}
	}
	active, _ := run.Cmd(5*time.Second, "systemctl", "show", "autoupdate.service", "-p", "ActiveState", "--value")
	st.Updating = active == "activating" || active == "active"
	c.mu.Lock()
	st.Check = c.check
	c.mu.Unlock()
	// A staged deployment means the update is already downloaded and waits for a reboot.
	if st.Staged != nil && st.Check.Available && st.Booted != nil && st.Staged.Version == st.Check.Version {
		st.Check.Available = false
	}
	return st
}

// Run checks the registry for a newer image without staging it.
func (c *Checker) Run() {
	c.mu.Lock()
	if c.check.Checking {
		c.mu.Unlock()
		return
	}
	c.check.Checking = true
	c.mu.Unlock()

	out, code := run.ExitCode(5*time.Minute, "rpm-ostree", "upgrade", "--check", "--unchanged-exit-77")
	res := Check{CheckedAt: time.Now().Unix()}
	switch code {
	case 77:
	case 0:
		res.Available = true
		res.Version, res.Digest = ParseCheck(out)
	default:
		res.Error = strings.TrimSpace(out)
		if res.Error == "" {
			res.Error = "rpm-ostree upgrade --check failed"
		}
	}
	c.mu.Lock()
	c.check = res
	c.mu.Unlock()
}

// CheckAsync starts a check and returns at once; the UI polls Status.
func (c *Checker) CheckAsync() error {
	if Fake {
		fakeCheck()
		return nil
	}
	if !Supported() {
		return errors.New("not an rpm-ostree system")
	}
	go c.Run()
	return nil
}

func Update() error {
	if Fake {
		fakeUpdate()
		return nil
	}
	if !Supported() {
		return errors.New("not an rpm-ostree system")
	}
	_, err := run.Cmd(10*time.Second, "systemctl", "start", "--no-block", "autoupdate.service")
	return err
}

// Rollback makes the previous deployment the default and reboots into it.
func Rollback() error {
	if !Supported() {
		return errors.New("not an rpm-ostree system")
	}
	if _, err := run.Cmd(2*time.Minute, "rpm-ostree", "rollback"); err != nil {
		return err
	}
	go func() {
		time.Sleep(time.Second)
		run.Cmd(30*time.Second, "systemctl", "reboot")
	}()
	return nil
}
