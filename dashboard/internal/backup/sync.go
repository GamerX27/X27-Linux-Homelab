package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
)

// Synced high-availability apps: like a replica, but only what changed travels. The node
// running the app sends its file list (SyncIndex); the member answers which files it lacks
// or has different (SyncNeed); then the node sends just those (SyncOut), with the list, so
// the member also deletes what's gone (SyncIn). Files land one by one through a rename, so
// a member never has half a file; while a sync is being applied a marker says so.

// SyncEntry is one file, folder or link, named like the move stream: docker/<folder>/… or
// volumes/<name>/….
type SyncEntry struct {
	Path  string `json:"p"`
	Type  byte   `json:"t"` // f, d, l
	Mode  uint32 `json:"m"`
	Size  int64  `json:"s,omitempty"`
	MTime int64  `json:"mt"` // unix nanoseconds
	UID   int    `json:"u"`
	GID   int    `json:"g"`
	Link  string `json:"l,omitempty"`
}

// SyncIndex is everything a synced app has, from the node running it.
type SyncIndex struct {
	SnapshotManifest
	Entries []SyncEntry `json:"entries"`
}

func syncWalk(dir, prefix string, out *[]SyncEntry) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		name := prefix
		if rel != "." {
			name += "/" + filepath.ToSlash(rel)
		}
		e := SyncEntry{Path: name, Mode: uint32(info.Mode().Perm()), MTime: info.ModTime().UnixNano()}
		if info.Mode()&fs.ModeSticky != 0 {
			e.Mode |= 0o1000
		}
		uid, gid := fileOwner(info)
		e.UID, e.GID = uid, gid
		switch {
		case info.IsDir():
			e.Type = 'd'
		case info.Mode()&fs.ModeSymlink != 0:
			e.Type = 'l'
			e.Link, _ = os.Readlink(p)
		case info.Mode().IsRegular():
			e.Type, e.Size = 'f', info.Size()
		default:
			return nil // sockets, devices: not data
		}
		*out = append(*out, e)
		return nil
	})
}

// syncRoots are the folders a synced app's entries live in on this node, by prefix.
func (m *Manager) syncRoots(o docker.Owner, man SnapshotManifest, create bool) (map[string]string, []string, error) {
	roots := map[string]string{}
	dir, err := docker.StackDir(o, man.Stack)
	if err != nil {
		return nil, nil, err
	}
	if _, err := os.Lstat(dir); err != nil {
		dir = filepath.Join(o.Root(), man.Folder)
	}
	roots["docker/"+man.Folder] = dir
	var made []string
	for _, v := range man.Volumes {
		if !validVolumeName(v.Name) {
			return nil, made, fmt.Errorf("bad volume name %q", v.Name)
		}
		vi, err := m.dk.VolumeInspect(v.Name)
		if err != nil {
			if !create {
				continue
			}
			if vi, err = m.dk.VolumeCreate(v.Name, v.Labels); err != nil {
				return nil, made, fmt.Errorf("creating volume %s: %w", v.Name, err)
			}
			made = append(made, v.Name)
		}
		roots["volumes/"+v.Name] = filepath.Clean(vi.Mountpoint)
	}
	return roots, made, nil
}

// MakeSyncIndex lists a synced app's files on the node running it. pause freezes its running
// containers until the returned func is called (after SyncOut), so the files sent match the
// list.
func (m *Manager) MakeSyncIndex(o docker.Owner, name string, pause bool) (SyncIndex, func(), error) {
	done := func() {}
	p, err := m.PlanMove(o, name)
	if err != nil {
		return SyncIndex{}, done, err
	}
	s, _, err := m.findStack(o, name)
	if err != nil {
		return SyncIndex{}, done, err
	}
	if pause {
		paused, err := m.pauseRunning(name)
		done = func() {
			for _, id := range paused {
				if err := m.dk.ContainerAction(id, "unpause"); err != nil {
					log.Printf("sync: unpausing a container of %s: %v", name, err)
				}
			}
		}
		if err != nil {
			done()
			return SyncIndex{}, func() {}, fmt.Errorf("pausing %s: %w", name, err)
		}
	}
	idx := SyncIndex{SnapshotManifest: SnapshotManifest{MoveManifest: MoveManifest{Version: MoveVersion, Stack: p.Stack, Folder: p.Folder,
		Host: hostname(), Owner: ManifestOwner{Name: o.Name, UID: o.UID, GID: o.GID}, Volumes: p.Volumes}, TakenAt: time.Now().Unix()}}
	if err := syncWalk(s.Dir, "docker/"+p.Folder, &idx.Entries); err != nil {
		done()
		return idx, func() {}, err
	}
	for _, v := range p.Volumes {
		vi, err := m.dk.VolumeInspect(v.Name)
		if err != nil {
			done()
			return idx, func() {}, fmt.Errorf("volume %s: %w", v.Name, err)
		}
		if err := syncWalk(vi.Mountpoint, "volumes/"+v.Name, &idx.Entries); err != nil {
			done()
			return idx, func() {}, fmt.Errorf("volume %s: %w", v.Name, err)
		}
	}
	return idx, done, nil
}

// syncOwner maps the sender's owner to this node's, as a move does, for the stack's folder.
func syncOwner(o docker.Owner, man SnapshotManifest, entry string, uid, gid int) (int, int) {
	if !strings.HasPrefix(entry, "docker/") {
		return uid, gid
	}
	if uid == man.Owner.UID {
		uid = o.UID
	}
	if gid == man.Owner.GID {
		gid = o.GID
	}
	return uid, gid
}

// locate is where an entry is on this node, inside its root.
func locate(roots map[string]string, entry string) (root, rel string, ok bool) {
	for prefix, r := range roots {
		if entry == prefix {
			return r, ".", true
		}
		if rest, found := strings.CutPrefix(entry, prefix+"/"); found {
			return r, path.Clean(rest), true
		}
	}
	return "", "", false
}

// SyncNeed is the files of idx this node lacks or has different (by type, size, time, mode,
// owner or link). Folders are only "needed" when missing; their modes come with SyncIn.
func (m *Manager) SyncNeed(o docker.Owner, idx SyncIndex) ([]string, error) {
	if err := checkSyncIndex(idx); err != nil {
		return nil, err
	}
	roots, _, err := m.syncRoots(o, idx.SnapshotManifest, false)
	if err != nil {
		return nil, err
	}
	need := []string{}
	for _, e := range idx.Entries {
		root, rel, ok := locate(roots, e.Path)
		if !ok {
			need = append(need, e.Path)
			continue
		}
		info, err := os.Lstat(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			need = append(need, e.Path)
			continue
		}
		uid, gid := fileOwner(info)
		wu, wg := syncOwner(o, idx.SnapshotManifest, e.Path, e.UID, e.GID)
		switch e.Type {
		case 'd':
			if !info.IsDir() {
				need = append(need, e.Path)
			}
		case 'l':
			if l, _ := os.Readlink(filepath.Join(root, filepath.FromSlash(rel))); info.Mode()&fs.ModeSymlink == 0 || l != e.Link {
				need = append(need, e.Path)
			}
		case 'f':
			if !info.Mode().IsRegular() || info.Size() != e.Size || info.ModTime().UnixNano() != e.MTime ||
				uint32(info.Mode().Perm()) != e.Mode&0o777 || uid != wu || gid != wg {
				need = append(need, e.Path)
			}
		}
	}
	return need, nil
}

func checkSyncIndex(idx SyncIndex) error {
	if idx.Version > MoveVersion {
		return fmt.Errorf("the other node is newer (format %d); update this one first", idx.Version)
	}
	if !docker.ValidStackName(idx.Stack) || !docker.ValidStackName(strings.ToLower(idx.Folder)) || strings.ContainsAny(idx.Folder, "/\\") {
		return errors.New("the stack or folder name isn't valid")
	}
	for _, e := range idx.Entries {
		if unsafeName(e.Path) {
			return fmt.Errorf("refused %q: it points outside", e.Path)
		}
	}
	return nil
}

// SyncOut streams the index and the files in need, from the node running the app.
func (m *Manager) SyncOut(ctx context.Context, w io.Writer, o docker.Owner, idx SyncIndex, need []string) error {
	s, _, err := m.findStack(o, idx.Stack)
	if err != nil {
		return err
	}
	roots := map[string]string{"docker/" + idx.Folder: s.Dir}
	for _, v := range idx.Volumes {
		vi, err := m.dk.VolumeInspect(v.Name)
		if err != nil {
			return fmt.Errorf("volume %s: %w", v.Name, err)
		}
		roots["volumes/"+v.Name] = vi.Mountpoint
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	js, _ := json.Marshal(idx)
	if err := tw.WriteHeader(&tar.Header{Name: "sync.json", Mode: 0o600, Size: int64(len(js)), ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := tw.Write(js); err != nil {
		return err
	}
	for _, n := range need {
		if err := ctx.Err(); err != nil {
			return err
		}
		root, rel, ok := locate(roots, n)
		if !ok {
			continue
		}
		p := filepath.Join(root, filepath.FromSlash(rel))
		info, err := os.Lstat(p)
		if errors.Is(err, os.ErrNotExist) {
			continue // gone since the list was made; the next sync deletes it
		}
		if err != nil {
			return err
		}
		if err := addEntry(tw, p, n, info); err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("%s: %w", n, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

// syncingPath marks a sync being applied; if it's still there when the app starts here, the
// copy may mix two moments.
func syncingPath(dir, stack string) string { return filepath.Join(dir, "ha-syncing-"+stack) }

// SyncInterrupted reports whether the last sync of stack into this node broke off halfway.
func SyncInterrupted(stateDir, stack string) bool {
	_, err := os.Stat(syncingPath(stateDir, stack))
	return err == nil
}

// SyncIn applies a SyncOut stream: changed files are written next to the old ones and
// renamed over them, then whatever isn't in the index anymore is deleted. It refuses while
// the app runs here.
func (m *Manager) SyncIn(o docker.Owner, r io.Reader, stateDir string) (SnapshotManifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return SnapshotManifest{}, errors.New("the stream isn't a sync")
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "sync.json" {
		return SnapshotManifest{}, errors.New("the stream doesn't start with sync.json")
	}
	var idx SyncIndex
	if err := json.NewDecoder(io.LimitReader(tr, 256<<20)).Decode(&idx); err != nil {
		return SnapshotManifest{}, fmt.Errorf("reading sync.json: %w", err)
	}
	man := idx.SnapshotManifest
	if err := checkSyncIndex(idx); err != nil {
		return man, err
	}
	if running, err := m.projectRunning(man.Stack); err != nil {
		return man, err
	} else if running {
		return man, ErrAppRunning
	}
	if err := mkdirOwned(o, o.Root()); err != nil {
		return man, err
	}
	roots, _, err := m.syncRoots(o, man, true)
	if err != nil {
		return man, err
	}
	folder := roots["docker/"+man.Folder]
	if err := os.MkdirAll(folder, 0o755); err != nil {
		return man, err
	}
	os.Chown(folder, o.UID, o.GID)
	marker := syncingPath(stateDir, man.Stack)
	os.WriteFile(marker, []byte(man.Host+"\n"), 0o600)

	byPath := make(map[string]SyncEntry, len(idx.Entries))
	for _, e := range idx.Entries {
		byPath[e.Path] = e
	}
	opened := map[string]*os.Root{}
	defer func() {
		for _, rt := range opened {
			rt.Close()
		}
	}()
	rootFor := func(entry string) (*os.Root, string, error) {
		dir, rel, ok := locate(roots, entry)
		if !ok {
			return nil, "", fmt.Errorf("unexpected entry %q", entry)
		}
		if rt := opened[dir]; rt != nil {
			return rt, rel, nil
		}
		rt, err := os.OpenRoot(dir)
		if err != nil {
			return nil, "", err
		}
		opened[dir] = rt
		return rt, rel, nil
	}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return man, fmt.Errorf("the sync broke off: %w", err)
		}
		if unsafeName(hdr.Name) {
			return man, fmt.Errorf("refused %q: it points outside", hdr.Name)
		}
		rt, rel, err := rootFor(strings.TrimSuffix(hdr.Name, "/"))
		if err != nil {
			return man, err
		}
		uid, gid := syncOwner(o, man, hdr.Name, hdr.Uid, hdr.Gid)
		// The index has the exact time (tar keeps whole seconds), so the next SyncNeed matches.
		if e, ok := byPath[strings.TrimSuffix(hdr.Name, "/")]; ok {
			hdr.ModTime = time.Unix(0, e.MTime)
		}
		if err := syncWrite(rt, rel, hdr, tr, uid, gid); err != nil {
			return man, fmt.Errorf("%s: %w", hdr.Name, err)
		}
	}

	// Deletions, deepest first, and folder modes from the index.
	keep := map[string]bool{}
	for _, e := range idx.Entries {
		keep[e.Path] = true
	}
	for prefix, dir := range roots {
		var gone []string
		filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			rel, _ := filepath.Rel(dir, p)
			if rel == "." {
				return nil
			}
			if !keep[prefix+"/"+filepath.ToSlash(rel)] {
				gone = append(gone, p)
				if d.IsDir() {
					return filepath.SkipDir
				}
			}
			return nil
		})
		for _, p := range slices.Backward(gone) {
			if err := os.RemoveAll(p); err != nil {
				return man, err
			}
		}
	}
	for _, e := range slices.Backward(idx.Entries) {
		if e.Type != 'd' {
			continue
		}
		rt, rel, err := rootFor(e.Path)
		if err != nil {
			continue
		}
		uid, gid := syncOwner(o, man, e.Path, e.UID, e.GID)
		mode := fs.FileMode(e.Mode) & fs.ModePerm
		if e.Mode&0o1000 != 0 {
			mode |= fs.ModeSticky
		}
		rt.Chmod(rel, mode)
		rt.Lchown(rel, uid, gid)
		t := time.Unix(0, e.MTime)
		rt.Chtimes(rel, t, t)
	}
	os.Remove(marker)
	return man, nil
}

// syncWrite puts one entry in place: a file through a temporary name and a rename, a link
// recreated, a folder made.
func syncWrite(rt *os.Root, rel string, hdr *tar.Header, r io.Reader, uid, gid int) error {
	mode := fs.FileMode(hdr.Mode) & fs.ModePerm
	if hdr.Mode&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	if dir := path.Dir(rel); dir != "." {
		if err := rt.MkdirAll(dir, 0o755); err != nil {
			return err
		}
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if rel == "." {
			return nil
		}
		if info, err := rt.Lstat(rel); err == nil && !info.IsDir() {
			if err := rt.Remove(rel); err != nil {
				return err
			}
		}
		if err := rt.MkdirAll(rel, 0o700); err != nil {
			return err
		}
		return nil
	case tar.TypeReg:
		tmp := path.Join(path.Dir(rel), ".x27-sync."+path.Base(rel))
		rt.Remove(tmp)
		f, err := rt.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			return err
		}
		_, err = io.Copy(f, r)
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err == nil {
			err = rt.Chmod(tmp, mode)
		}
		if err == nil {
			err = rt.Lchown(tmp, uid, gid)
		}
		if err == nil {
			err = rt.Chtimes(tmp, hdr.ModTime, hdr.ModTime)
		}
		if err == nil {
			if info, lerr := rt.Lstat(rel); lerr == nil && info.IsDir() {
				err = rt.RemoveAll(rel)
			}
		}
		if err == nil {
			err = rt.Rename(tmp, rel)
		}
		if err != nil {
			rt.Remove(tmp)
		}
		return err
	case tar.TypeSymlink:
		if info, err := rt.Lstat(rel); err == nil {
			if info.IsDir() {
				if err := rt.RemoveAll(rel); err != nil {
					return err
				}
			} else if err := rt.Remove(rel); err != nil {
				return err
			}
		}
		if err := rt.Symlink(hdr.Linkname, rel); err != nil {
			return err
		}
		return rt.Lchown(rel, uid, gid)
	}
	return nil
}

func fileOwner(info fs.FileInfo) (int, int) {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return int(st.Uid), int(st.Gid)
	}
	return -1, -1
}
