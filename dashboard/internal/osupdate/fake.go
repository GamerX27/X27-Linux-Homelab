package osupdate

import (
	"sync"
	"time"
)

// Fake is only for `dashboard serve --dev` with DASHBOARD_FAKE_OS=1: it pretends to be an
// rpm-ostree node with an update available, so the UI's update flows can be tried on a
// machine that isn't one. Nothing is installed and nothing reboots.
var Fake bool

var fake = struct {
	sync.Mutex
	version, next string
	updatingSince time.Time
	checkedAt     int64
}{version: "44.20260921", next: "44.20260928"}

func fakeStatus() Status {
	fake.Lock()
	defer fake.Unlock()
	// An "update" takes 20 s, then the node is on the new version.
	if !fake.updatingSince.IsZero() && time.Since(fake.updatingSince) > 20*time.Second {
		fake.version, fake.next, fake.updatingSince = fake.next, "", time.Time{}
	}
	booted := Deployment{Version: fake.version, Booted: true, Image: "ghcr.io/gamerx27/x27-linux-homelab:44", Timestamp: time.Now().Add(-7 * 24 * time.Hour).Unix()}
	st := Status{Supported: true, Deployments: []Deployment{booted}, Updating: !fake.updatingSince.IsZero()}
	st.Booted = &st.Deployments[0]
	st.Check = Check{CheckedAt: fake.checkedAt, Available: fake.next != "", Version: fake.next}
	return st
}

func fakeCheck() {
	fake.Lock()
	fake.checkedAt = time.Now().Unix()
	fake.Unlock()
}

func fakeUpdate() {
	fake.Lock()
	if fake.next != "" && fake.updatingSince.IsZero() {
		fake.updatingSince = time.Now()
	}
	fake.Unlock()
}
