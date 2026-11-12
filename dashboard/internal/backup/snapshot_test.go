package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
)

func TestSnapshotRoundTrip(t *testing.T) {
	u, _ := user.Current()
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// The holder: ~/docker/notes with an sqlite file and a named volume.
	srcHome := t.TempDir()
	src := docker.Owner{Name: u.Username, Home: srcHome, UID: uid, GID: gid}
	write(filepath.Join(srcHome, "docker/notes/compose.yml"), "services: {}\n")
	write(filepath.Join(srcHome, "docker/notes/data/notes.db"), "row 1")
	fsrc := &fakeDocker{dir: t.TempDir(), volumes: map[string]docker.Volume{}}
	vmp := filepath.Join(fsrc.dir, "volumes", "notes_cache")
	write(filepath.Join(vmp, "c.bin"), "v1")
	fsrc.volumes["notes_cache"] = docker.Volume{Name: "notes_cache", Driver: "local", Mountpoint: vmp,
		Labels: map[string]string{"com.docker.compose.project": "notes"}}
	fsrc.containers = []docker.Container{{ID: "c1", Names: []string{"/notes-app-1"}, State: "exited",
		Labels: map[string]string{"com.docker.compose.project": "notes"}, Mounts: []docker.Mount{{Type: "volume", Name: "notes_cache"}}}}
	msrc := &Manager{dk: startFake(t, fsrc)}

	// A member that already has an older copy.
	dstHome := t.TempDir()
	dst := docker.Owner{Name: u.Username, Home: dstHome, UID: uid, GID: gid}
	write(filepath.Join(dstHome, "docker/notes/compose.yml"), "services: {}\n")
	write(filepath.Join(dstHome, "docker/notes/data/notes.db"), "old")
	write(filepath.Join(dstHome, "docker/notes/data/stale.tmp"), "gone after the swap")
	fdst := &fakeDocker{dir: t.TempDir(), volumes: map[string]docker.Volume{}}
	mdst := &Manager{dk: startFake(t, fdst)}

	snap := func() []byte {
		var buf bytes.Buffer
		if err := msrc.SnapshotOut(context.Background(), &buf, src, "notes"); err != nil {
			t.Fatal(err)
		}
		return buf.Bytes()
	}
	first := snap()
	man, err := mdst.SnapshotIn(dst, bytes.NewReader(first))
	if err != nil {
		t.Fatal(err)
	}
	if man.Stack != "notes" || man.TakenAt == 0 || man.Host == "" {
		t.Fatalf("manifest %+v", man)
	}
	if b, _ := os.ReadFile(filepath.Join(dstHome, "docker/notes/data/notes.db")); string(b) != "row 1" {
		t.Fatalf("folder not replaced: %q", b)
	}
	if _, err := os.Stat(filepath.Join(dstHome, "docker/notes/data/stale.tmp")); err == nil {
		t.Fatal("the old copy's files are still there")
	}
	v := fdst.volumes["notes_cache"]
	if b, _ := os.ReadFile(filepath.Join(v.Mountpoint, "c.bin")); string(b) != "v1" {
		t.Fatal("volume not filled")
	}
	if e, _ := os.ReadDir(replicaDir(dst)); len(e) != 0 {
		t.Fatalf("staging left behind: %v", e)
	}

	// A newer copy replaces the volume's contents too.
	write(filepath.Join(vmp, "c.bin"), "v2")
	if _, err := mdst.SnapshotIn(dst, bytes.NewReader(snap())); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(v.Mountpoint, "c.bin")); string(b) != "v2" {
		t.Fatal("volume not replaced")
	}

	// A running app with a stopped container: only the running one is paused, then unpaused.
	fsrc.containers = append(fsrc.containers, docker.Container{ID: "c2", Names: []string{"/notes-web-1"}, State: "running",
		Labels: map[string]string{"com.docker.compose.project": "notes"}})
	snap()
	if strings.Join(fsrc.paused, " ") != "/containers/c2/pause /containers/c2/unpause" || fsrc.containers[1].State != "running" {
		t.Fatalf("pause calls %v, state %s", fsrc.paused, fsrc.containers[1].State)
	}

	// A big app isn't paused at all.
	fsrc.paused = nil
	SnapshotPauseLimit = 1
	snap()
	SnapshotPauseLimit = 256 << 20
	if len(fsrc.paused) != 0 {
		t.Fatalf("paused a big app: %v", fsrc.paused)
	}

	// Half a stream changes nothing; a running app is never replaced.
	write(filepath.Join(srcHome, "docker/notes/data/notes.db"), "row 2")
	two := snap()
	if _, err := mdst.SnapshotIn(dst, bytes.NewReader(two[:len(two)/2])); err == nil {
		t.Fatal("half a stream accepted")
	}
	if b, _ := os.ReadFile(filepath.Join(dstHome, "docker/notes/data/notes.db")); string(b) != "row 1" {
		t.Fatal("a broken copy changed the live data")
	}
	fdst.containers = []docker.Container{{ID: "c9", Names: []string{"/notes-app-1"}, State: "running",
		Labels: map[string]string{"com.docker.compose.project": "notes"}}}
	if _, err := mdst.SnapshotIn(dst, bytes.NewReader(two)); !errors.Is(err, ErrAppRunning) {
		t.Fatalf("replaced a running app: %v", err)
	}
}
