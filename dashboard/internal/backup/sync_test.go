package backup

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
)

func TestSyncRoundTrip(t *testing.T) {
	u, _ := user.Current()
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	write := func(p, s string) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	srcHome, dstHome, state := t.TempDir(), t.TempDir(), t.TempDir()
	src := docker.Owner{Name: u.Username, Home: srcHome, UID: uid, GID: gid}
	dst := docker.Owner{Name: u.Username, Home: dstHome, UID: uid, GID: gid}
	write(filepath.Join(srcHome, "docker/jf/compose.yml"), "services: {}\n")
	write(filepath.Join(srcHome, "docker/jf/config/library.db"), "v1")
	write(filepath.Join(srcHome, "docker/jf/config/old.log"), "old")
	os.Symlink("library.db", filepath.Join(srcHome, "docker/jf/config/link"))
	fsrc := &fakeDocker{dir: t.TempDir(), volumes: map[string]docker.Volume{}}
	vmp := filepath.Join(fsrc.dir, "volumes", "jf_data")
	write(filepath.Join(vmp, "a.bin"), "A")
	fsrc.volumes["jf_data"] = docker.Volume{Name: "jf_data", Driver: "local", Mountpoint: vmp, Labels: map[string]string{"com.docker.compose.project": "jf"}}
	fsrc.containers = []docker.Container{{ID: "c1", Names: []string{"/jf-app-1"}, State: "running",
		Labels: map[string]string{"com.docker.compose.project": "jf"}, Mounts: []docker.Mount{{Type: "volume", Name: "jf_data"}}}}
	msrc := &Manager{dk: startFake(t, fsrc)}
	fdst := &fakeDocker{dir: t.TempDir(), volumes: map[string]docker.Volume{}}
	mdst := &Manager{dk: startFake(t, fdst)}

	sync := func() []string {
		t.Helper()
		idx, done, err := msrc.MakeSyncIndex(src, "jf", true)
		if err != nil {
			t.Fatal(err)
		}
		if fsrc.containers[0].State != "paused" {
			t.Fatal("not paused while listing")
		}
		need, err := mdst.SyncNeed(dst, idx)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := msrc.SyncOut(context.Background(), &buf, src, idx, need); err != nil {
			t.Fatal(err)
		}
		done()
		if fsrc.containers[0].State != "running" {
			t.Fatal("not unpaused")
		}
		if _, err := mdst.SyncIn(dst, bytes.NewReader(buf.Bytes()), state); err != nil {
			t.Fatal(err)
		}
		return need
	}
	first := sync()
	if b, _ := os.ReadFile(filepath.Join(dstHome, "docker/jf/config/library.db")); string(b) != "v1" {
		t.Fatal("folder not synced")
	}
	if l, _ := os.Readlink(filepath.Join(dstHome, "docker/jf/config/link")); l != "library.db" {
		t.Fatal("link")
	}
	if b, _ := os.ReadFile(filepath.Join(fdst.volumes["jf_data"].Mountpoint, "a.bin")); string(b) != "A" {
		t.Fatal("volume not synced")
	}
	if len(first) < 6 {
		t.Fatalf("first sync sent only %v", first)
	}

	// Nothing changed: nothing sent. Then one change, one new file, one deletion.
	if again := sync(); len(again) != 0 {
		t.Fatalf("resent %v", again)
	}
	time.Sleep(10 * time.Millisecond)
	write(filepath.Join(srcHome, "docker/jf/config/library.db"), "v2")
	write(filepath.Join(srcHome, "docker/jf/config/new/x"), "x")
	os.Remove(filepath.Join(srcHome, "docker/jf/config/old.log"))
	got := sync()
	if len(got) != 3 { // library.db, new/, new/x
		t.Fatalf("sent %v", got)
	}
	if b, _ := os.ReadFile(filepath.Join(dstHome, "docker/jf/config/library.db")); string(b) != "v2" {
		t.Fatal("change not synced")
	}
	if _, err := os.Stat(filepath.Join(dstHome, "docker/jf/config/old.log")); err == nil {
		t.Fatal("deleted file still there")
	}
	if SyncInterrupted(state, "jf") {
		t.Fatal("marker left after a good sync")
	}

	// A broken stream leaves the marker; a running app refuses.
	time.Sleep(10 * time.Millisecond)
	write(filepath.Join(srcHome, "docker/jf/config/library.db"), "v3 with more data to make the stream longer")
	idx, done, _ := msrc.MakeSyncIndex(src, "jf", false)
	need, _ := mdst.SyncNeed(dst, idx)
	var buf bytes.Buffer
	msrc.SyncOut(context.Background(), &buf, src, idx, need)
	done()
	if _, err := mdst.SyncIn(dst, bytes.NewReader(buf.Bytes()[:buf.Len()-30]), state); err == nil {
		t.Fatal("half a stream accepted")
	}
	if !SyncInterrupted(state, "jf") {
		t.Fatal("no marker after a broken sync")
	}
	fdst.containers = []docker.Container{{ID: "c9", Names: []string{"/jf-app-1"}, State: "running", Labels: map[string]string{"com.docker.compose.project": "jf"}}}
	if _, err := mdst.SyncIn(dst, bytes.NewReader(buf.Bytes()), state); !errors.Is(err, ErrAppRunning) {
		t.Fatalf("synced into a running app: %v", err)
	}
}
