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
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/storage"
)

// Moving a stack to another node: the main node streams MoveOut on the old node straight
// into MoveIn on the new one. The stream is a .tar.gz with move.json first, then the stack's
// folder under docker/<folder>/ and each named volume it uses under volumes/<name>/.

const MoveVersion = 1

type MoveVolume struct {
	Name   string            `json:"name"`
	Labels map[string]string `json:"labels,omitempty"`
	Size   int64             `json:"size"`
}

type MoveManifest struct {
	Version int           `json:"version"`
	Stack   string        `json:"stack"`
	Folder  string        `json:"folder"` // its folder's name in ~/docker
	Host    string        `json:"host"`
	Owner   ManifestOwner `json:"owner"`
	Volumes []MoveVolume  `json:"volumes"`
}

// MovePlan is what moving a stack would take, from the node it's on.
type MovePlan struct {
	Stack    string       `json:"stack"`
	Folder   string       `json:"folder"`
	Running  bool         `json:"running"`
	Size     int64        `json:"size"` // folder and volumes
	Volumes  []MoveVolume `json:"volumes"`
	Ports    []int        `json:"ports"`    // published on the host
	Shares   []string     `json:"shares"`   // shares under /var/mnt it uses; the new node must have them too
	Warnings []string     `json:"warnings"` // what won't come along
}

// MoveCheck is what the new node says about receiving a stack.
type MoveCheck struct {
	Free     int64    `json:"free"` // bytes free where the stack and volumes would go
	Problems []string `json:"problems"`
	Warnings []string `json:"warnings"`
}

// ignoredBinds are host paths apps commonly mount that exist on every node.
var ignoredBinds = []string{"/var/run/docker.sock", "/run/docker.sock", "/etc/localtime", "/etc/timezone", "/dev", "/proc", "/sys"}

func (m *Manager) findStack(o docker.Owner, name string) (docker.Stack, []docker.Container, error) {
	cs, err := m.dk.Containers()
	if err != nil {
		return docker.Stack{}, nil, err
	}
	for _, s := range rootStacks(o, cs) {
		if s.Name == name {
			var mine []docker.Container
			for _, c := range cs {
				if c.Project() == name {
					mine = append(mine, c)
				}
			}
			return s, mine, nil
		}
	}
	return docker.Stack{}, nil, fmt.Errorf("there's no stack %q in %s; only stacks there can be moved", name, o.Root())
}

func dirSize(dir string) int64 {
	var n int64
	filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			if info, err := d.Info(); err == nil {
				n += info.Size()
			}
		}
		return nil
	})
	return n
}

// PlanMove works out what moving a stack takes: its folder, the named volumes its containers
// use or compose made for it, and what can't come along (bind mounts outside its folder,
// volumes of other drivers).
func (m *Manager) PlanMove(o docker.Owner, name string) (MovePlan, error) {
	s, cs, err := m.findStack(o, name)
	if err != nil {
		return MovePlan{}, err
	}
	p := MovePlan{Stack: s.Name, Folder: filepath.Base(s.Dir), Running: s.Running > 0, Size: dirSize(s.Dir),
		Volumes: []MoveVolume{}, Ports: []int{}, Shares: []string{}, Warnings: []string{}}
	vols := map[string]bool{}
	for _, c := range cs {
		for _, mt := range c.Mounts {
			switch mt.Type {
			case "volume":
				vols[mt.Name] = true
			case "bind":
				src := filepath.Clean(mt.Source)
				if src == s.Dir || strings.HasPrefix(src, s.Dir+"/") || slices.ContainsFunc(ignoredBinds, func(b string) bool {
					return src == b || strings.HasPrefix(src, b+"/")
				}) {
					continue
				}
				// A share at the same /var/mnt/<name> on the new node (a cluster share, say)
				// needs no copy; CheckMoveIn makes sure it's there.
				if rest, ok := strings.CutPrefix(src, storage.MountRoot()+"/"); ok {
					share, _, _ := strings.Cut(rest, "/")
					if !slices.Contains(p.Shares, share) {
						p.Shares = append(p.Shares, share)
					}
					continue
				}
				w := fmt.Sprintf("%s is mounted from %s, outside the stack's folder; it isn't copied", c.Name(), src)
				if !slices.Contains(p.Warnings, w) {
					p.Warnings = append(p.Warnings, w)
				}
			}
		}
		for _, port := range c.Ports {
			if port.PublicPort > 0 && !slices.Contains(p.Ports, port.PublicPort) {
				p.Ports = append(p.Ports, port.PublicPort)
			}
		}
	}
	if all, err := m.dk.Volumes(); err == nil {
		for _, v := range all {
			if v.Project() == name {
				vols[v.Name] = true
			}
		}
	}
	for _, name := range slices.Sorted(func(yield func(string) bool) {
		for k := range vols {
			if !yield(k) {
				return
			}
		}
	}) {
		v, err := m.dk.VolumeInspect(name)
		if err != nil {
			p.Warnings = append(p.Warnings, fmt.Sprintf("volume %s: %v; it isn't copied", name, err))
			continue
		}
		if v.Driver != "local" {
			p.Warnings = append(p.Warnings, fmt.Sprintf("volume %s uses the %s driver; it isn't copied", name, v.Driver))
			continue
		}
		if isAnonymous(name) {
			p.Warnings = append(p.Warnings, fmt.Sprintf("an unnamed volume (%s…) isn't copied: the new node makes a fresh one. Name it in the compose file to move its data", name[:12]))
			continue
		}
		size := dirSize(v.Mountpoint)
		p.Size += size
		p.Volumes = append(p.Volumes, MoveVolume{Name: v.Name, Labels: v.Labels, Size: size})
	}
	sort.Ints(p.Ports)
	return p, nil
}

// isAnonymous: a volume named with 64 hex digits is one Docker made for an image's VOLUME.
// Compose makes a new one on the new node, so copying it wouldn't help.
func isAnonymous(name string) bool {
	if len(name) != 64 {
		return false
	}
	for _, r := range name {
		if !strings.ContainsRune("0123456789abcdef", r) {
			return false
		}
	}
	return true
}

func freeBytes(dir string) int64 {
	var st syscall.Statfs_t
	for d := dir; ; d = filepath.Dir(d) {
		if err := syscall.Statfs(d, &st); err == nil {
			return int64(st.Bavail) * int64(st.Bsize)
		}
		if d == "/" {
			return -1
		}
	}
}

// CheckMoveIn says whether this node can take a stack: its folder and volumes mustn't be here
// already, there must be room, and its ports should be free.
func (m *Manager) CheckMoveIn(o docker.Owner, p MovePlan) MoveCheck {
	c := MoveCheck{Problems: []string{}, Warnings: []string{}}
	if !docker.ValidStackName(strings.ToLower(p.Folder)) || strings.ContainsAny(p.Folder, "/\\") {
		c.Problems = append(c.Problems, "the stack's folder name isn't valid")
		return c
	}
	if _, err := os.Lstat(filepath.Join(o.Root(), p.Folder)); err == nil {
		c.Problems = append(c.Problems, fmt.Sprintf("%s already exists here; remove it (or that stack) first", filepath.Join(o.Root(), p.Folder)))
	}
	cs, err := m.dk.Containers()
	if err != nil {
		c.Problems = append(c.Problems, "Docker isn't reachable: "+err.Error())
		return c
	}
	for _, x := range cs {
		if x.Project() == p.Stack {
			c.Problems = append(c.Problems, fmt.Sprintf("a stack named %s already has containers here", p.Stack))
			break
		}
	}
	for _, v := range p.Volumes {
		if _, err := m.dk.VolumeInspect(v.Name); err == nil {
			c.Problems = append(c.Problems, fmt.Sprintf("a volume named %s already exists here", v.Name))
		}
	}
	for _, sh := range p.Shares {
		if !shareAvailable(sh) {
			c.Problems = append(c.Problems, fmt.Sprintf("the stack uses %s, but nothing is mounted there on this node; add the share here first (or make it a cluster share on this node too)",
				filepath.Join(storage.MountRoot(), sh)))
		}
	}
	used := map[int]string{}
	for _, x := range cs {
		if x.State != "running" {
			continue
		}
		for _, port := range x.Ports {
			if port.PublicPort > 0 {
				used[port.PublicPort] = x.Name()
			}
		}
	}
	for _, port := range p.Ports {
		if who, ok := used[port]; ok {
			c.Problems = append(c.Problems, fmt.Sprintf("port %d is already used here by %s", port, who))
		}
	}
	c.Free = freeBytes(o.Root())
	var volSize int64
	for _, v := range p.Volumes {
		volSize += v.Size
	}
	folderSize := p.Size - volSize
	if c.Free >= 0 {
		need := folderSize
		if volFree := freeBytes(dockerRoot()); volFree >= 0 && sameFS(o.Root(), dockerRoot()) {
			need += volSize
		} else if volFree >= 0 && volFree < volSize+volSize/20 {
			c.Problems = append(c.Problems, fmt.Sprintf("Docker's volumes need %s, but only %s is free there", human(volSize), human(volFree)))
		}
		if c.Free < need+need/20 {
			c.Problems = append(c.Problems, fmt.Sprintf("it needs %s, but only %s is free here", human(need), human(c.Free)))
		}
	}
	return c
}

func dockerRoot() string { return "/var/lib/docker" }

// shareAvailable is storage.Available; moved in tests.
var shareAvailable = storage.Available

func sameFS(a, b string) bool {
	var sa, sb syscall.Stat_t
	if syscall.Stat(existing(a), &sa) != nil || syscall.Stat(existing(b), &sb) != nil {
		return true
	}
	return sa.Dev == sb.Dev
}

func existing(p string) string {
	for ; p != "/"; p = filepath.Dir(p) {
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "/"
}

func human(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}

// MoveOut streams a stack to w. With stop, a running stack is stopped first and left
// stopped: the main node starts it again if the move fails.
func (m *Manager) MoveOut(ctx context.Context, w io.Writer, o docker.Owner, name string, stop bool) error {
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
	if stop && p.Running {
		log.Printf("move: stopping %s", name)
		if out, err := m.dk.StackAction(o, name, "stop"); err != nil {
			return fmt.Errorf("stopping %s: %w %s", name, err, strings.TrimSpace(out))
		}
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	man := MoveManifest{Version: MoveVersion, Stack: p.Stack, Folder: p.Folder, Host: hostname(),
		Owner: ManifestOwner{Name: o.Name, UID: o.UID, GID: o.GID}, Volumes: p.Volumes}
	js, _ := json.MarshalIndent(man, "", "  ")
	if err := tw.WriteHeader(&tar.Header{Name: "move.json", Mode: 0o600, Size: int64(len(js)), ModTime: time.Now(), Typeflag: tar.TypeReg}); err != nil {
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

// walkInto adds dir and everything in it as prefix/...
func walkInto(ctx context.Context, tw *tar.Writer, dir, prefix string) error {
	return filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
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
		return addEntry(tw, p, name, info)
	})
}

// MoveIn receives a stack from MoveOut: the folder goes in ~/docker (its owner becomes this
// node's user) and each volume is created and filled, keeping numeric owners. Whatever
// was made is removed again if anything fails. It doesn't start the stack.
func (m *Manager) MoveIn(o docker.Owner, r io.Reader) (MoveManifest, error) {
	var man MoveManifest
	if !m.running.TryLock() {
		return man, ErrRunning
	}
	defer m.running.Unlock()
	gz, err := gzip.NewReader(r)
	if err != nil {
		return man, errors.New("the stream isn't a stack being moved")
	}
	tr := tar.NewReader(gz)
	hdr, err := tr.Next()
	if err != nil || hdr.Name != "move.json" {
		return man, errors.New("the stream doesn't start with move.json")
	}
	if err := json.NewDecoder(io.LimitReader(tr, 1<<20)).Decode(&man); err != nil {
		return man, fmt.Errorf("reading move.json: %w", err)
	}
	if man.Version > MoveVersion {
		return man, fmt.Errorf("the other node is newer (move format %d); update this one first", man.Version)
	}
	check := m.CheckMoveIn(o, MovePlan{Stack: man.Stack, Folder: man.Folder, Volumes: man.Volumes})
	if len(check.Problems) > 0 {
		return man, errors.New(strings.Join(check.Problems, "; "))
	}
	if err := mkdirOwned(o, o.Root()); err != nil {
		return man, err
	}

	// Undo: the folder and the volumes made so far.
	var made []string
	folder := filepath.Join(o.Root(), man.Folder)
	ok := false
	defer func() {
		if ok {
			return
		}
		os.RemoveAll(folder)
		for _, v := range made {
			if err := m.dk.VolumeRemove(v); err != nil {
				log.Printf("move: removing volume %s after a failed move: %v", v, err)
			}
		}
	}()

	home, err := os.OpenRoot(o.Root())
	if err != nil {
		return man, err
	}
	defer home.Close()
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
	unpackers := map[string]*moveUnpacker{"docker/" + man.Folder: {root: home, sub: man.Folder, ids: ownerMap}}
	for _, v := range man.Volumes {
		if !validVolumeName(v.Name) {
			return man, fmt.Errorf("bad volume name %q", v.Name)
		}
		vi, err := m.dk.VolumeCreate(v.Name, v.Labels)
		if err != nil {
			return man, fmt.Errorf("creating volume %s: %w", v.Name, err)
		}
		made = append(made, v.Name)
		root, err := os.OpenRoot(vi.Mountpoint)
		if err != nil {
			return man, fmt.Errorf("volume %s: %w", v.Name, err)
		}
		defer root.Close()
		unpackers["volumes/"+v.Name] = &moveUnpacker{root: root, ids: same}
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
	ok = true
	return man, nil
}

func validVolumeName(n string) bool {
	if n == "" || len(n) > 255 || strings.ContainsAny(n, "/\\") || n == "." || n == ".." {
		return false
	}
	return docker.ValidID(n)
}

// moveUnpacker writes entries under root (in sub, if set), like Unpack: through os.Root, so
// nothing lands outside; symlinks are recreated, never followed; folder modes come last.
type moveUnpacker struct {
	root *os.Root
	sub  string
	ids  func(uid, gid int) (int, int)
	dirs []dirMeta
}

type dirMeta struct {
	name     string
	mode     fs.FileMode
	uid, gid int
	mtime    time.Time
}

func (u *moveUnpacker) add(hdr *tar.Header, rel string, r io.Reader) error {
	name := rel
	if u.sub != "" {
		name = path.Join(u.sub, rel)
	}
	uid, gid := u.ids(hdr.Uid, hdr.Gid)
	mode := fs.FileMode(hdr.Mode) & fs.ModePerm
	if hdr.Mode&0o1000 != 0 {
		mode |= fs.ModeSticky
	}
	switch hdr.Typeflag {
	case tar.TypeDir:
		if name != "." {
			if err := u.root.MkdirAll(name, 0o700); err != nil {
				return err
			}
		}
		u.dirs = append(u.dirs, dirMeta{name, mode, uid, gid, hdr.ModTime})
		return nil
	case tar.TypeReg:
		return writeEntry(u.root, name, func() error {
			f, err := u.root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
			if err != nil {
				return err
			}
			_, err = io.Copy(f, r)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err == nil {
				err = u.root.Chmod(name, mode)
			}
			if err == nil {
				err = u.root.Lchown(name, uid, gid)
			}
			if err == nil {
				err = u.root.Chtimes(name, hdr.ModTime, hdr.ModTime)
			}
			return err
		})
	case tar.TypeSymlink:
		return writeEntry(u.root, name, func() error {
			if err := u.root.Symlink(hdr.Linkname, name); err != nil {
				return err
			}
			return u.root.Lchown(name, uid, gid)
		})
	}
	return nil
}

func (u *moveUnpacker) finish() error {
	for i := len(u.dirs) - 1; i >= 0; i-- {
		d := u.dirs[i]
		err := u.root.Chmod(d.name, d.mode)
		if err == nil {
			err = u.root.Lchown(d.name, d.uid, d.gid)
		}
		if err == nil {
			err = u.root.Chtimes(d.name, d.mtime, d.mtime)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", d.name, err)
		}
	}
	return nil
}

// AbortMoveIn removes what a move brought here, after it failed to start: the stack's
// containers, its folder and its volumes.
func (m *Manager) AbortMoveIn(o docker.Owner, stack, folder string, volumes []string) error {
	var errs []string
	if out, err := m.dk.RemoveStack(o, stack, false); err != nil {
		log.Printf("move: removing %s: %v %s", stack, err, out)
	}
	if folder == "" || strings.ContainsAny(folder, "/\\") || folder == "." || folder == ".." {
		return errors.New("bad folder")
	}
	if err := os.RemoveAll(filepath.Join(o.Root(), folder)); err != nil {
		errs = append(errs, err.Error())
	}
	for _, v := range volumes {
		if err := m.dk.VolumeRemove(v); err != nil {
			errs = append(errs, fmt.Sprintf("volume %s: %v", v, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}

// FinishMoveOut runs on the old node once the stack runs on the new one: it marks the folder
// as moved, or with remove, takes the stack down and deletes its folder and volumes.
func (m *Manager) FinishMoveOut(o docker.Owner, stack, to string, remove bool, volumes []string) error {
	s, _, err := m.findStack(o, stack)
	if err != nil {
		return err
	}
	if !remove {
		return os.WriteFile(filepath.Join(s.Dir, docker.MovedFile), []byte(to+"\n"+time.Now().UTC().Format(time.RFC3339)+"\n"), 0o644)
	}
	if out, err := m.dk.RemoveStack(o, stack, true); err != nil {
		return fmt.Errorf("removing %s: %w %s", stack, err, strings.TrimSpace(out))
	}
	var errs []string
	for _, v := range volumes {
		if err := m.dk.VolumeRemove(v); err != nil {
			errs = append(errs, fmt.Sprintf("volume %s: %v", v, err))
		}
	}
	if len(errs) > 0 {
		return errors.New(strings.Join(errs, "; "))
	}
	return nil
}
