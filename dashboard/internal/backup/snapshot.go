package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
)

// Replicated high-availability apps: the node running the app streams SnapshotOut into
// SnapshotIn on every other member of the virtual IP, on a timer. The stream is the move
// stream's layout (docker/<folder>/, volumes/<name>/) with snapshot.json first. A member
// unpacks it next to the live copy and swaps it in only while the app isn't running there.

// ErrAppRunning: a replica can't replace an app that runs on this node.
var ErrAppRunning = errors.New("the app runs on this node; a replica can't replace it")

type SnapshotManifest struct {
	MoveManifest
	TakenAt int64 `json:"takenAt"` // when the copy was taken, on the sending node's clock
}

// replicaDir holds a replica being received, inside ~/docker so it can be swapped in with a
// rename. It has no compose file at its top, so it isn't listed as a stack.
func replicaDir(o docker.Owner) string { return filepath.Join(o.Root(), ".x27-replicas") }

func (m *Manager) projectRunning(name string) (bool, error) {
	cs, err := m.dk.Containers()
	if err != nil {
		return false, err
	}
	for _, c := range cs {
		if c.Project() == name && (c.State == "running" || c.State == "paused" || c.State == "restarting") {
			return true, nil
		}
	}
	return false, nil
}

// SnapshotOut streams a copy of a stack to w. A running stack is paused for the copy, so
// files (an sqlite database, say) aren't caught halfway through a write, and unpaused after.
func (m *Manager) SnapshotOut(ctx context.Context, w io.Writer, o docker.Owner, name string) error {
	p, err := m.PlanMove(o, name)
	if err != nil {
		return err
	}
	s, _, err := m.findStack(o, name)
	if err != nil {
		return err
	}
	if !m.running.TryLock() {
		return ErrRunning
	}
	defer m.running.Unlock()
	if p.Running {
		if out, err := m.dk.StackAction(o, name, "pause"); err != nil {
			return fmt.Errorf("pausing %s: %w %s", name, err, strings.TrimSpace(out))
		}
		defer func() {
			if out, err := m.dk.StackAction(o, name, "unpause"); err != nil {
				log.Printf("replica: unpausing %s: %v %s", name, err, out)
			}
		}()
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	man := SnapshotManifest{MoveManifest: MoveManifest{Version: MoveVersion, Stack: p.Stack, Folder: p.Folder, Host: hostname(),
		Owner: ManifestOwner{Name: o.Name, UID: o.UID, GID: o.GID}, Volumes: p.Volumes}, TakenAt: time.Now().Unix()}
	js, _ := json.MarshalIndent(man, "", "  ")
	if err := tw.WriteHeader(&tar.Header{Name: "snapshot.json", Mode: 0o600, Size: int64(len(js)), ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(js); err != nil {
		return err
	}
	if err := walkInto(ctx, tw, s.Dir, "docker/"+p.Folder); err != nil {
		return fmt.Errorf("%s: %w", s.Dir, err)
	}
	for _, v := range p.Volumes {
		vi, err := m.dk.VolumeInspect(v.Name)
		if err != nil {
			return fmt.Errorf("volume %s: %w", v.Name, err)
		}
		if err := walkInto(ctx, tw, vi.Mountpoint, "volumes/"+v.Name); err != nil {
			return fmt.Errorf("volume %s: %w", v.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// SnapshotIn receives a copy from SnapshotOut and swaps it in for this node's copy of the
// stack and its volumes. Nothing changes unless the whole stream arrived.
func (m *Manager) SnapshotIn(o docker.Owner, r io.Reader) (SnapshotManifest, error) {
	var man SnapshotManifest
	if !m.running.TryLock() {
		return man, ErrRunning
	}
	defer m.running.Unlock()
	gz, err := gzip.NewReader(r)
	if err != nil {
		return man, errors.New("the stream isn't a replica")
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "snapshot.json" {
		return man, errors.New("the stream doesn't start with snapshot.json")
	}
	if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&man); err != nil {
		return man, fmt.Errorf("reading snapshot.json: %w", err)
	}
	if man.Version > MoveVersion {
		return man, fmt.Errorf("the other node is newer (format %d); update this one first", man.Version)
	}
	if !docker.ValidStackName(man.Stack) || !docker.ValidStackName(strings.ToLower(man.Folder)) || strings.ContainsAny(man.Folder, "/\\") {
		return man, errors.New("the replica's stack or folder name isn't valid")
	}
	if running, err := m.projectRunning(man.Stack); err != nil {
		return man, err
	} else if running {
		return man, ErrAppRunning
	}
	if err := mkdirOwned(o, o.Root()); err != nil {
		return man, err
	}
	if err := os.MkdirAll(replicaDir(o), 0o700); err != nil {
		return man, err
	}
	rev := strconv.FormatInt(man.TakenAt, 10)
	stageName := man.Folder + "." + rev
	stage := filepath.Join(replicaDir(o), stageName)
	os.RemoveAll(stage)

	// Undo: the staging folders and the volumes made for this replica.
	var made []string
	stages := []string{stage}
	ok := false
	defer func() {
		for _, s := range stages {
			os.RemoveAll(s)
		}
		if ok {
			return
		}
		for _, v := range made {
			m.dk.VolumeRemove(v)
		}
	}()

	rdir, err := os.OpenRoot(replicaDir(o))
	if err != nil {
		return man, err
	}
	defer rdir.Close()
	ownerMap := func(uid, gid int) (int, int) {
		if uid == man.Owner.UID {
			uid = o.UID
		}
		if gid == man.Owner.GID {
			gid = o.GID
		}
		return uid, gid
	}
	same := func(uid, gid int) (int, int) { return uid, gid }
	if err := rdir.Mkdir(stageName, 0o700); err != nil {
		return man, err
	}
	unpackers := map[string]*moveUnpacker{"docker/" + man.Folder: {root: rdir, sub: stageName, ids: ownerMap}}
	type volSwap struct{ live, stage string }
	var swaps []volSwap
	for _, v := range man.Volumes {
		if !validVolumeName(v.Name) {
			return man, fmt.Errorf("bad volume name %q", v.Name)
		}
		vi, err := m.dk.VolumeInspect(v.Name)
		if err != nil {
			if vi, err = m.dk.VolumeCreate(v.Name, v.Labels); err != nil {
				return man, fmt.Errorf("creating volume %s: %w", v.Name, err)
			}
			made = append(made, v.Name)
		}
		vstage := filepath.Clean(vi.Mountpoint) + ".x27-replica"
		os.RemoveAll(vstage)
		if err := os.Mkdir(vstage, 0o755); err != nil {
			return man, fmt.Errorf("volume %s: %w", v.Name, err)
		}
		stages = append(stages, vstage)
		root, err := os.OpenRoot(vstage)
		if err != nil {
			return man, fmt.Errorf("volume %s: %w", v.Name, err)
		}
		defer root.Close()
		unpackers["volumes/"+v.Name] = &moveUnpacker{root: root, ids: same}
		swaps = append(swaps, volSwap{live: filepath.Clean(vi.Mountpoint), stage: vstage})
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return man, fmt.Errorf("the copy broke off: %w", err)
		}
		if unsafeName(hdr.Name) {
			return man, fmt.Errorf("refused %q: it points outside", hdr.Name)
		}
		var u *moveUnpacker
		var rel string
		for prefix, x := range unpackers {
			if hdr.Name == prefix || hdr.Name == prefix+"/" {
				u, rel = x, "."
				break
			}
			if r, found := strings.CutPrefix(hdr.Name, prefix+"/"); found {
				u, rel = x, path.Clean(r)
				break
			}
		}
		if u == nil {
			return man, fmt.Errorf("unexpected entry %q", hdr.Name)
		}
		if err := u.add(hdr, rel, tr); err != nil {
			return man, fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}
	for _, u := range unpackers {
		if err := u.finish(); err != nil {
			return man, err
		}
	}
	// Someone may have started it meanwhile.
	if running, err := m.projectRunning(man.Stack); err != nil || running {
		return man, ErrAppRunning
	}

	// Swap: the live copy goes aside, the replica takes its place, the old copy is deleted.
	live, err := docker.StackDir(o, man.Stack)
	if err != nil {
		return man, err
	}
	if _, err := os.Lstat(live); err != nil {
		live = filepath.Join(o.Root(), man.Folder)
	}
	if err := swapIn(live, stage, filepath.Join(replicaDir(o), stageName+".old")); err != nil {
		return man, err
	}
	for _, s := range swaps {
		if err := swapIn(s.live, s.stage, s.live+".x27-old"); err != nil {
			return man, err
		}
	}
	ok = true
	return man, nil
}

// swapIn puts stage where live is, with two renames, and deletes the old live copy.
func swapIn(live, stage, old string) error {
	os.RemoveAll(old)
	hadLive := true
	if err := os.Rename(live, old); err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("moving %s aside: %w", live, err)
		}
		hadLive = false
	}
	if err := os.Rename(stage, live); err != nil {
		if hadLive {
			os.Rename(old, live)
		}
		return fmt.Errorf("putting the replica at %s: %w", live, err)
	}
	if hadLive {
		os.RemoveAll(old)
	}
	return nil
}
