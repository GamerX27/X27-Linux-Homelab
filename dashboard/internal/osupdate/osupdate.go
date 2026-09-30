// Package osupdate shows which image the node runs and whether a newer one is out.
// It only reads through rpm-ostree; installing goes through the autoupdate units
// (autoupdate-run), so an update from the dashboard behaves exactly like a scheduled one:
// staged, Gotify message, reboot, checked after the reboot. autoupdate-run records every
// step in StateDir, which is how the dashboard follows an update across the reboot.
package osupdate

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

const (
	checkEvery = 6 * time.Hour
	// A failed check is retried sooner: right after boot the network is up before DNS is
	// (a DNS container on the LAN may still be starting), and the error would otherwise
	// stay on the page for six hours.
	retryEvery = 2 * time.Minute
	retries    = 5
)

// StateDir is where autoupdate-run keeps state.json and last.log. Moved in tests.
var StateDir = "/var/lib/autoupdate"

// The phases of an update, as autoupdate-run writes them. Verifying is the dashboard's own:
// the node rebooted but autoupdate-verify.service hasn't checked the image yet.
const (
	PhaseChecking    = "checking"
	PhaseDownloading = "downloading"
	PhaseStaged      = "staged"
	PhaseRebooting   = "rebooting"
	PhaseVerifying   = "verifying"
	PhaseDone        = "done"
	PhaseFailed      = "failed"
	PhaseUpToDate    = "uptodate"
)

// Job is the latest update run on this node (StateDir/state.json).
type Job struct {
	ID             string `json:"id"`
	Phase          string `json:"phase"`
	BootID         string `json:"bootId"` // the boot it was started (or rebooted) from
	From           string `json:"from"`
	FromName       string `json:"fromName"`
	To             string `json:"to"`
	ToName         string `json:"toName"`
	TargetDigest   string `json:"targetDigest"`
	TargetChecksum string `json:"targetChecksum"`
	Error          string `json:"error,omitempty"`
	StartedAt      int64  `json:"startedAt"`
	UpdatedAt      int64  `json:"updatedAt"`
	FinishedAt     int64  `json:"finishedAt,omitempty"`
}

// Running reports whether the job is still going: downloading, or rebooting into the update.
func (j *Job) Running() bool {
	if j == nil {
		return false
	}
	switch j.Phase {
	case PhaseChecking, PhaseDownloading, PhaseRebooting, PhaseVerifying:
		return true
	}
	return false
}

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
	Updating    bool         `json:"updating"` // an update is downloading or rebooting
	Job         *Job         `json:"job"`      // the latest update run, nil if there never was one
	Progress    string       `json:"progress"` // what the running update is doing, from its log
	BootID      string       `json:"bootId"`
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

// NewChecker checks shortly after start and then every six hours, retrying a failed check
// every two minutes a few times first.
func NewChecker() *Checker {
	c := &Checker{}
	if Supported() {
		go func() {
			time.Sleep(30 * time.Second)
			for {
				c.Run()
				for i := 0; i < retries && c.failed(); i++ {
					time.Sleep(retryEvery)
					c.Run()
				}
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
	st.BootID = BootID()
	active, _ := run.Cmd(5*time.Second, "systemctl", "show", "-p", "ActiveState", "--value",
		"autoupdate.service", "autoupdate-stage.service", "autoupdate-apply.service", "autoupdate-verify.service")
	unitActive := AnyActive(active)
	st.Job = ReadJob(st.BootID, unitActive)
	st.Updating = unitActive || st.Job.Running()
	if st.Job.Running() {
		st.Progress = Progress(logTail())
	}
	c.mu.Lock()
	st.Check = c.check
	c.mu.Unlock()
	// A staged deployment means the update is already downloaded and waits for a reboot.
	if st.Staged != nil && st.Check.Available && st.Booted != nil && st.Staged.Version == st.Check.Version {
		st.Check.Available = false
	}
	return st
}

// AnyActive reports whether any line of `systemctl show -p ActiveState --value` output is
// "active" or "activating". A substring match won't do: "inactive" contains "activ".
func AnyActive(out string) bool {
	for _, l := range strings.Split(out, "\n") {
		switch strings.TrimSpace(l) {
		case "active", "activating", "reloading":
			return true
		}
	}
	return false
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

func (c *Checker) failed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.check.Error != ""
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

func BootID() string {
	b, _ := os.ReadFile("/proc/sys/kernel/random/boot_id")
	return strings.TrimSpace(string(b))
}

// ReadJob reads state.json as seen from this boot. A job that says it's downloading while no
// autoupdate unit runs was cut off; one that's rebooting from another boot is waiting for
// autoupdate-verify.service. Nil when there's no state yet.
func ReadJob(bootID string, unitActive bool) *Job {
	if Fake {
		return fakeJob()
	}
	b, err := os.ReadFile(filepath.Join(StateDir, "state.json"))
	if err != nil {
		return nil
	}
	var j Job
	if json.Unmarshal(b, &j) != nil {
		return nil
	}
	return adjustJob(&j, bootID, unitActive)
}

func adjustJob(j *Job, bootID string, unitActive bool) *Job {
	switch j.Phase {
	case PhaseChecking, PhaseDownloading:
		if !unitActive {
			j.Phase = PhaseFailed
			if j.BootID != bootID {
				j.Error = "The update was cut off by a reboot before it was staged."
			} else {
				j.Error = "The update run stopped before it was staged; see the log."
			}
		}
	case PhaseRebooting:
		if j.BootID != bootID {
			j.Phase = PhaseVerifying
		}
	}
	return j
}

func logTail() string {
	f, err := os.Open(filepath.Join(StateDir, "last.log"))
	if err != nil {
		return ""
	}
	defer f.Close()
	if fi, err := f.Stat(); err == nil && fi.Size() > 64<<10 {
		f.Seek(-64<<10, io.SeekEnd)
	}
	b, _ := io.ReadAll(f)
	return string(b)
}

var (
	layersNeeded = regexp.MustCompile(`layers needed: (\d+)`)
	fetching     = regexp.MustCompile(`^Fetching (?:ostree chunk|layer) `)
)

// Progress turns rpm-ostree's output into one line: "Downloading layer 12 of 65" while it
// fetches image layers, otherwise its last line.
func Progress(log string) string {
	lines := strings.Split(strings.ReplaceAll(log, "\r", "\n"), "\n")
	total, fetched := 0, 0
	last := ""
	for _, l := range lines {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		last = l
		if m := layersNeeded.FindStringSubmatch(l); m != nil {
			n, _ := strconv.Atoi(m[1])
			total += n
		}
		if fetching.MatchString(l) {
			fetched++
		}
	}
	if total > 0 && fetched > 0 && fetched <= total {
		return "Downloading layer " + strconv.Itoa(fetched) + " of " + strconv.Itoa(total)
	}
	if len(last) > 160 {
		last = last[:160] + "…"
	}
	return last
}

// ReadLog returns the update log from offset on and the offset to ask for next. A log that
// got shorter was started over by a new run, so it's read from the start.
func ReadLog(offset int64) (string, int64) {
	if Fake {
		return fakeLog(offset)
	}
	f, err := os.Open(filepath.Join(StateDir, "last.log"))
	if err != nil {
		return "", 0
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return "", 0
	}
	if offset < 0 || offset > fi.Size() {
		offset = 0
	}
	f.Seek(offset, io.SeekStart)
	b, _ := io.ReadAll(io.LimitReader(f, 256<<10))
	return string(b), offset + int64(len(b))
}

func startUnit(unit string) error {
	if !Supported() {
		return errors.New("not an rpm-ostree system")
	}
	_, err := run.Cmd(10*time.Second, "systemctl", "start", "--no-block", unit)
	return err
}

// Update stages an update and reboots into it, like the timer does.
func Update() error {
	if Fake {
		return fakeStage(true)
	}
	return startUnit("autoupdate.service")
}

// Stage downloads and stages an update without rebooting.
func Stage() error {
	if Fake {
		return fakeStage(false)
	}
	if j := ReadJob(BootID(), false); j.Running() && j.Phase != PhaseVerifying {
		return errors.New("an update is already running")
	}
	return startUnit("autoupdate-stage.service")
}

// Apply reboots into the staged update.
func Apply() error {
	if Fake {
		return fakeApply()
	}
	return startUnit("autoupdate-apply.service")
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
