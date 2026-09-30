package osupdate

import (
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
)

// Fake is only for `dashboard serve --dev` with DASHBOARD_FAKE_OS=1: it pretends to be an
// rpm-ostree node with an update available, so the UI's update flows can be tried on a
// machine that isn't one. An update checks for 2 s, downloads for 12 s, and a reboot keeps
// the node down (FakeDown) from 3 s to 18 s after it starts. With FakeFail
// (DASHBOARD_FAKE_OS=fail) the node comes back on the old image. Nothing is installed and
// nothing reboots.
var (
	Fake     bool
	FakeFail bool
)

const fakeLayers = 10

var fake = struct {
	sync.Mutex
	version, next string
	boot          int
	job           *Job
	started       time.Time // the stage step started
	rebooted      time.Time // the reboot started
	autoApply     bool      // Update(): reboot once staged
	checkedAt     int64
	log           strings.Builder
}{version: "44.20260921", next: "44.20260928", boot: 1}

func fakeBootID() string { return fmt.Sprintf("fake-boot-%d", fake.boot) }

func fakeLogf(format string, a ...any) { fmt.Fprintf(&fake.log, format+"\n", a...) }

// fakeAdvance moves the fake job along its timeline. Called with fake locked.
func fakeAdvance() {
	j := fake.job
	if j == nil {
		return
	}
	now := time.Now()
	switch j.Phase {
	case PhaseChecking:
		if now.Sub(fake.started) > 2*time.Second {
			j.Phase, j.To = PhaseDownloading, fake.next
			fakeLogf("Pulling manifest: ostree-image-signed:docker://ghcr.io/gamerx27/x27-linux-homelab:44")
			fakeLogf("ostree chunk layers needed: %d (1.2 GB)", fakeLayers)
		}
	case PhaseDownloading:
		for n := strings.Count(fake.log.String(), "Fetching "); n < fakeLayers && now.Sub(fake.started) > time.Duration(3+n)*time.Second; n++ {
			fakeLogf("Fetching ostree chunk sha256:%064x (120 MB)... done", n)
		}
		if now.Sub(fake.started) > 14*time.Second {
			j.Phase, j.ToName, j.TargetChecksum = PhaseStaged, "X27-Linux Homelab 44 ("+fake.next+")", "fake-"+fake.next
			fakeLogf("Staging deployment... done")
			fakeLogf("Staged %s.", j.ToName)
			if fake.autoApply {
				fakeApplyLocked()
			}
		}
	case PhaseRebooting:
		if now.Sub(fake.rebooted) > 18*time.Second {
			fake.boot++
			j.BootID, j.FinishedAt = fakeBootID(), now.Unix()
			if FakeFail {
				j.Phase, j.Error = PhaseFailed, "fake came back on "+fake.version+", not the update to "+fake.next+"."
				fakeLogf("%s", j.Error)
			} else {
				fake.version, fake.next = fake.next, ""
				j.Phase = PhaseDone
				fakeLogf("Running %s.", j.ToName)
			}
		}
	}
	j.UpdatedAt = now.Unix()
}

// FakeDown reports whether the fake node is "rebooting": the dev server answers 503 then.
func FakeDown() bool {
	if !Fake {
		return false
	}
	fake.Lock()
	defer fake.Unlock()
	fakeAdvance()
	if fake.job == nil || fake.job.Phase != PhaseRebooting {
		return false
	}
	d := time.Since(fake.rebooted)
	return d > 3*time.Second && d <= 18*time.Second
}

func fakeJob() *Job {
	fake.Lock()
	defer fake.Unlock()
	fakeAdvance()
	if fake.job == nil {
		return nil
	}
	j := *fake.job
	return &j
}

func fakeStatus() Status {
	fake.Lock()
	defer fake.Unlock()
	fakeAdvance()
	booted := Deployment{Version: fake.version, Booted: true, Image: "ghcr.io/gamerx27/x27-linux-homelab:44",
		Digest: "sha256:fake-" + fake.version, Timestamp: time.Now().Add(-7 * 24 * time.Hour).Unix()}
	st := Status{Supported: true, Deployments: []Deployment{booted}, BootID: fakeBootID()}
	if j := fake.job; j != nil && (j.Phase == PhaseStaged || j.Phase == PhaseRebooting) {
		st.Deployments = append([]Deployment{{Version: fake.next, Staged: true, Image: booted.Image,
			Digest: "sha256:fake-" + fake.next, Timestamp: time.Now().Unix()}}, st.Deployments...)
		st.Staged = &st.Deployments[0]
	}
	st.Booted = &st.Deployments[len(st.Deployments)-1]
	if fake.job != nil {
		j := *fake.job
		st.Job = &j
		st.Updating = j.Running()
		if j.Running() {
			st.Progress = Progress(fake.log.String())
		}
	}
	st.Check = Check{CheckedAt: fake.checkedAt, Available: fake.next != "" && st.Staged == nil, Version: fake.next}
	return st
}

func fakeCheck() {
	fake.Lock()
	fake.checkedAt = time.Now().Unix()
	fake.Unlock()
}

func fakeStage(apply bool) error {
	fake.Lock()
	defer fake.Unlock()
	fakeAdvance()
	if fake.job.Running() {
		return errors.New("an update is already running")
	}
	now := time.Now()
	fake.log.Reset()
	fake.started, fake.autoApply = now, apply
	fake.job = &Job{ID: fmt.Sprint(now.UnixNano()), Phase: PhaseChecking, BootID: fakeBootID(),
		From: fake.version, FromName: "X27-Linux Homelab 44 (" + fake.version + ")", StartedAt: now.Unix(), UpdatedAt: now.Unix()}
	fakeLogf("Checking for an update…")
	if fake.next == "" {
		fake.job.Phase = PhaseUpToDate
		fakeLogf("No update available.")
	}
	return nil
}

func fakeApply() error {
	fake.Lock()
	defer fake.Unlock()
	fakeAdvance()
	if fake.job == nil || fake.job.Phase != PhaseStaged {
		return errors.New("there's no staged update to reboot into")
	}
	fakeApplyLocked()
	return nil
}

func fakeApplyLocked() {
	fake.rebooted = time.Now()
	fake.job.Phase = PhaseRebooting
	fakeLogf("%s → %s\n\nRebooting now.", fake.job.FromName, fake.job.ToName)
}

func fakeLog(offset int64) (string, int64) {
	fake.Lock()
	defer fake.Unlock()
	fakeAdvance()
	s := fake.log.String()
	if offset < 0 || offset > int64(len(s)) {
		offset = 0
	}
	return s[offset:], int64(len(s))
}
