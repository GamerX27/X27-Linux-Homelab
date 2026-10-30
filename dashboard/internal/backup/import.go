package backup

import (
	"archive/tar"
	"compress/gzip"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"maps"
	"os"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
)

// importTTL is how long an uploaded export waits to be applied before it's deleted.
const importTTL = time.Hour

// ImportApp is one app in an uploaded export, and how it would land here.
type ImportApp struct {
	Name    string `json:"name"`
	Data    bool   `json:"data"` // the whole folder, not just the config files
	Size    int64  `json:"size"`
	Exists  bool   `json:"exists"` // ~/docker/<name> is already here
	Running bool   `json:"running"`
}

// Preview is what the import page shows after the upload.
type Preview struct {
	ID      string            `json:"id"`
	Host    string            `json:"host"`
	Taken   int64             `json:"taken"`
	Apps    []ImportApp       `json:"apps"`
	Parts   []string          `json:"parts"`   // server parts in the file, in the order they're applied
	Details map[string]string `json:"details"` // each part in a few words
}

// ImportRequest is what the user ticked in the preview.
type ImportRequest struct {
	ID      string   `json:"id"`
	Apps    []string `json:"apps"`
	Replace bool     `json:"replace"` // write over apps that are already here
	Start   bool     `json:"start"`   // docker compose up -d each restored app
	Parts   []string `json:"parts"`
}

type pendingImport struct {
	preview Preview
	man     Manifest
	path    string
	timer   *time.Timer
}

func importDir() string { return filepath.Join(config.StateDir, "import") }

// dropImport deletes the pending upload (only the one with id, when id isn't empty).
func (m *Manager) dropImport(id string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.imp
	if p == nil || (id != "" && p.preview.ID != id) {
		return
	}
	p.timer.Stop()
	os.Remove(p.path)
	m.imp = nil
}

// Upload stores an export to import and reads what's in it. Only the newest upload is kept,
// for an hour or until it's applied.
func (m *Manager) Upload(o docker.Owner, body io.Reader) (Preview, error) {
	m.dropImport("")
	if err := os.MkdirAll(importDir(), 0o700); err != nil {
		return Preview{}, err
	}
	// Leftovers from before a restart.
	if old, _ := filepath.Glob(filepath.Join(importDir(), "*.tar.gz")); len(old) > 0 {
		for _, f := range old {
			os.Remove(f)
		}
	}
	b := make([]byte, 8)
	rand.Read(b)
	id := hex.EncodeToString(b)
	p := filepath.Join(importDir(), id+".tar.gz")
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return Preview{}, err
	}
	_, err = io.Copy(f, body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(p)
		return Preview{}, fmt.Errorf("receiving the file: %w", err)
	}
	f, err = os.Open(p)
	if err != nil {
		os.Remove(p)
		return Preview{}, err
	}
	man, apps, err := ReadExport(f)
	f.Close()
	if err != nil {
		os.Remove(p)
		return Preview{}, err
	}
	pv := Preview{ID: id, Host: man.Host, Taken: man.Taken, Apps: apps, Parts: man.Present(), Details: man.Details()}
	if pv.Parts == nil {
		pv.Parts = []string{}
	}
	m.markExisting(o, pv.Apps)
	m.mu.Lock()
	m.imp = &pendingImport{preview: pv, man: man, path: p, timer: time.AfterFunc(importTTL, func() { m.dropImport(id) })}
	m.mu.Unlock()
	return pv, nil
}

// markExisting notes which apps are already in ~/docker, and running.
func (m *Manager) markExisting(o docker.Owner, apps []ImportApp) {
	var stacks []docker.Stack
	if m.dk != nil {
		if cs, err := m.dk.Containers(); err == nil {
			stacks = rootStacks(o, cs)
		}
	}
	for i := range apps {
		if _, err := os.Lstat(filepath.Join(o.Root(), apps[i].Name)); err == nil {
			apps[i].Exists = true
		}
		for _, s := range stacks {
			if filepath.Base(s.Dir) == apps[i].Name && s.Running > 0 {
				apps[i].Running = true
			}
		}
	}
}

// ReadExport reads server.json and lists the apps under docker/ in an export.
func ReadExport(r io.Reader) (Manifest, []ImportApp, error) {
	var man Manifest
	gz, err := gzip.NewReader(r)
	if err != nil {
		return man, nil, errors.New("this isn't a .tar.gz export file")
	}
	tr := tar.NewReader(gz)
	apps := map[string]*ImportApp{}
	sawManifest := false
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return man, nil, fmt.Errorf("reading the file: %w", err)
		}
		if hdr.Name == "server.json" {
			if err := json.NewDecoder(io.LimitReader(tr, 16<<20)).Decode(&man); err != nil {
				return man, nil, fmt.Errorf("reading server.json: %w", err)
			}
			sawManifest = true
			continue
		}
		rest, ok := strings.CutPrefix(hdr.Name, "docker/")
		if !ok {
			continue
		}
		app, sub, _ := strings.Cut(strings.TrimSuffix(rest, "/"), "/")
		if !docker.ValidStackName(strings.ToLower(app)) {
			continue
		}
		a := apps[app]
		if a == nil {
			a = &ImportApp{Name: app}
			apps[app] = a
		}
		if hdr.Typeflag == tar.TypeReg {
			a.Size += hdr.Size
		}
		// Anything deeper than the top of the folder, or not a small file, is data.
		if sub != "" && (strings.Contains(sub, "/") || hdr.Typeflag != tar.TypeReg || hdr.Size > maxConfigFile) {
			a.Data = true
		}
	}
	if !sawManifest {
		return man, nil, errors.New("there's no server.json in this file: it isn't an export from Backup → Export & import")
	}
	if man.Version > ManifestVersion {
		return man, nil, fmt.Errorf("this export is format %d; update this server to import it", man.Version)
	}
	out := []ImportApp{}
	for _, a := range apps {
		out = append(out, *a)
	}
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return man, out, nil
}

// unsafeName refuses absolute paths and any .. part, before os.Root gets a say.
func unsafeName(name string) bool {
	if strings.HasPrefix(name, "/") || strings.Contains(name, "\\") {
		return true
	}
	return slices.Contains(strings.Split(name, "/"), "..")
}

// Unpack writes the apps in want from an export into root (~/docker). Every write goes
// through os.Root, so a path with .., an absolute one, or one through a symlink can't land
// outside it; symlinks are recreated, never followed. Modes (without setuid and setgid),
// numeric owners and mtimes are kept; uidMap turns the exporting user's ids into this
// user's. Files already there are overwritten, others are left alone. It returns each
// app's first error; an app that failed gets no more entries.
func Unpack(r io.Reader, root *os.Root, want map[string]bool, uidMap func(uid, gid int) (int, int)) map[string]error {
	errs := map[string]error{}
	gz, err := gzip.NewReader(r)
	if err != nil {
		for a := range want {
			errs[a] = err
		}
		return errs
	}
	tr := tar.NewReader(gz)
	type dirMeta struct {
		name     string
		mode     fs.FileMode
		uid, gid int
		mtime    time.Time
	}
	var dirs []dirMeta
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			for a := range want {
				if errs[a] == nil {
					errs[a] = fmt.Errorf("reading the file: %w", err)
				}
			}
			break
		}
		rest, ok := strings.CutPrefix(hdr.Name, "docker/")
		if !ok {
			continue
		}
		app, _, _ := strings.Cut(rest, "/")
		if !want[app] || errs[app] != nil {
			continue
		}
		if unsafeName(hdr.Name) {
			errs[app] = fmt.Errorf("refused %q: it points outside the app's folder", hdr.Name)
			continue
		}
		name := path.Clean(rest)
		uid, gid := uidMap(hdr.Uid, hdr.Gid)
		mode := fs.FileMode(hdr.Mode) & fs.ModePerm
		if hdr.Mode&0o1000 != 0 {
			mode |= fs.ModeSticky
		}
		var werr error
		switch hdr.Typeflag {
		case tar.TypeDir:
			if werr = root.MkdirAll(name, 0o700); werr == nil {
				dirs = append(dirs, dirMeta{name, mode, uid, gid, hdr.ModTime})
			}
		case tar.TypeReg:
			werr = writeEntry(root, name, func() error {
				f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
				if err != nil {
					return err
				}
				_, err = io.Copy(f, tr)
				if cerr := f.Close(); err == nil {
					err = cerr
				}
				if err == nil {
					err = root.Chmod(name, mode)
				}
				if err == nil {
					err = root.Lchown(name, uid, gid)
				}
				if err == nil {
					err = root.Chtimes(name, hdr.ModTime, hdr.ModTime)
				}
				return err
			})
		case tar.TypeSymlink:
			werr = writeEntry(root, name, func() error {
				if err := root.Symlink(hdr.Linkname, name); err != nil {
					return err
				}
				return root.Lchown(name, uid, gid)
			})
		default:
			continue // hard links, devices and pipes aren't in exports
		}
		if werr != nil {
			errs[app] = fmt.Errorf("%s: %w", hdr.Name, werr)
		}
	}
	// Folders last, deepest first: writing into them changed their mtimes.
	for i := len(dirs) - 1; i >= 0; i-- {
		d := dirs[i]
		app, _, _ := strings.Cut(d.name, "/")
		if errs[app] != nil {
			continue
		}
		err := root.Chmod(d.name, d.mode)
		if err == nil {
			err = root.Lchown(d.name, d.uid, d.gid)
		}
		if err == nil {
			err = root.Chtimes(d.name, d.mtime, d.mtime)
		}
		if err != nil {
			errs[app] = fmt.Errorf("docker/%s: %w", d.name, err)
		}
	}
	return errs
}

// writeEntry makes the parent folders, takes away a file or symlink already at name (never
// what a symlink points to), and runs write.
func writeEntry(root *os.Root, name string, write func() error) error {
	if dir := path.Dir(name); dir != "." {
		if err := root.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	if st, err := root.Lstat(name); err == nil {
		if st.IsDir() {
			return errors.New("there's a folder with that name here")
		}
		if err := root.Remove(name); err != nil {
			return err
		}
	}
	return write()
}

// StartImport applies an uploaded export in the background, as a job like a backup run:
// the apps, then updates, shares, the dashboard settings, the network, and the dashboard
// port last.
func (m *Manager) StartImport(o docker.Owner, req ImportRequest) (Job, error) {
	parts, err := ParseParts(req.Parts)
	if err != nil {
		return Job{}, err
	}
	m.mu.Lock()
	p := m.imp
	m.mu.Unlock()
	if p == nil || p.preview.ID != req.ID {
		return Job{}, errors.New("the uploaded file is gone (it's kept for an hour); upload it again")
	}
	for _, a := range req.Apps {
		if !slices.ContainsFunc(p.preview.Apps, func(x ImportApp) bool { return x.Name == a }) {
			return Job{}, fmt.Errorf("there's no app %q in the file", a)
		}
	}
	man := p.man
	if !parts.Dashboard {
		man.Dashboard = nil
	}
	if !parts.Shares {
		man.Shares = nil
	}
	if !parts.Updates {
		man.Updates = nil
	}
	if !parts.Network {
		man.Network = nil
	}
	if len(req.Apps) == 0 && len(man.Present()) == 0 {
		return Job{}, errors.New("tick at least one app or part to import")
	}
	f, err := os.Open(p.path)
	if err != nil {
		return Job{}, errors.New("the uploaded file is gone; upload it again")
	}
	if !m.running.TryLock() {
		f.Close()
		return Job{}, ErrRunning
	}
	m.mu.Lock()
	m.job = &Job{ID: time.Now().UnixMilli(), Kind: "import", Phase: "starting", Pending: slices.Clone(req.Apps), StartedAt: time.Now().Unix()}
	m.log = nil
	j := *m.job
	m.mu.Unlock()
	go func() {
		defer m.running.Unlock()
		defer m.dropImport(req.ID)
		defer f.Close()
		m.runImport(o, f, req, man)
	}()
	return j, nil
}

func (m *Manager) runImport(o docker.Owner, f *os.File, req ImportRequest, man Manifest) {
	m.logf("Importing the export of %s from %s", man.Host, time.Unix(man.Taken, 0).Format("2006-01-02 15:04"))
	var done, failed []string // failed: apps and parts
	step := func(name string, err error) {
		if err != nil {
			m.logf("%s: failed: %v", name, err)
			failed = append(failed, name)
			m.setJob(func(j *Job) { j.Failed++ })
		} else {
			m.setJob(func(j *Job) { j.Done++ })
		}
	}

	restored := 0
	if len(req.Apps) > 0 {
		m.phase("apps")
		restored = m.importApps(o, f, req, man, step)
	}
	if man.Updates != nil {
		m.phase("updates")
		m.setJob(func(j *Job) { j.Current = "" })
		err := m.importUpdates(man.Updates)
		step("updates", err)
		if err == nil {
			done = append(done, "updates")
		}
	}
	if man.Shares != nil {
		m.phase("shares")
		err := m.importShares(man.Shares)
		step("shares", err)
		if err == nil {
			done = append(done, "shares")
		}
	}
	port, restart := 0, false
	if man.Dashboard != nil {
		m.phase("dashboard")
		var err error
		port, restart, err = m.importDashboard(o, man.Dashboard)
		step("dashboard settings", err)
		if err == nil {
			done = append(done, "dashboard settings")
		}
	}
	waiting := false
	if man.Network != nil {
		m.phase("network")
		var err error
		waiting, err = m.importNetwork(man.Network)
		step("network", err)
		if err == nil {
			done = append(done, "network")
		}
	}
	if port != 0 {
		if err := config.SetPort(port); err != nil {
			step("dashboard port", err)
		} else {
			restart = true
			m.logf("dashboard: moving to port %d: open https://<this server>:%d once it's back", port, port)
		}
	}

	summary := fmt.Sprintf("Imported %d app%s", restored, map[bool]string{true: "", false: "s"}[restored == 1])
	if len(done) > 0 {
		summary += ", " + strings.Join(done, ", ")
	}
	if waiting {
		summary += "; network waiting to be kept"
		m.logf("Network change waiting: keep it under System → Settings within 2 minutes, or it's undone.")
	}
	if port != 0 {
		summary += fmt.Sprintf("; the dashboard moves to port %d", port)
	}
	m.logf("%s.", summary)
	m.mu.Lock()
	j := m.job
	j.FinishedAt, j.Current, j.Pending, j.Summary = time.Now().Unix(), "", nil, summary
	j.Phase = "done"
	if len(failed) > 0 {
		j.Phase, j.Error = "failed", "failed: "+strings.Join(failed, ", ")
	}
	j.Waiting, j.Port = waiting, port
	log.Printf("import: %s; %d failed", summary, len(failed))
	m.mu.Unlock()
	if restart && m.Server.Restart != nil {
		m.Server.Restart()
	}
}

// importApps unpacks the ticked apps into ~/docker. One that's already there is skipped
// unless req.Replace; then a running one is stopped first. With req.Start, or when it was
// running, each restored app is started with docker compose up -d.
func (m *Manager) importApps(o docker.Owner, f *os.File, req ImportRequest, man Manifest, step func(string, error)) (restored int) {
	if err := mkdirOwned(o, o.Root()); err != nil {
		for _, a := range req.Apps {
			step(a, err)
		}
		return 0
	}
	var stacks []docker.Stack
	if m.dk != nil {
		if cs, err := m.dk.Containers(); err == nil {
			stacks = rootStacks(o, cs)
		}
	}
	stackOf := func(folder string) *docker.Stack {
		for i := range stacks {
			if filepath.Base(stacks[i].Dir) == folder {
				return &stacks[i]
			}
		}
		return nil
	}
	want := map[string]bool{}
	wasRunning := map[string]bool{}
	for _, a := range req.Apps {
		m.setJob(func(j *Job) { j.Current = a })
		if _, err := os.Lstat(filepath.Join(o.Root(), a)); err == nil {
			if !req.Replace {
				m.logf("%s: already in %s; skipped (tick Replace to write over it)", a, o.Root())
				m.setJob(func(j *Job) {
					j.Pending = slices.DeleteFunc(j.Pending, func(p string) bool { return p == a })
				})
				continue
			}
			if s := stackOf(a); s != nil && s.Running > 0 && m.dk != nil {
				m.logf("%s: stopping it before its files are replaced", a)
				out, err := m.dk.StackAction(o, s.Name, "stop")
				m.logf("%s", out)
				if err != nil {
					step(a, fmt.Errorf("stopping: %w", err))
					continue
				}
				wasRunning[a] = true
			}
		}
		want[a] = true
	}
	if len(want) == 0 {
		return 0
	}

	m.setJob(func(j *Job) { j.Current = "" })
	m.logf("Unpacking %s into %s", strings.Join(slices.Sorted(maps.Keys(want)), ", "), o.Root())
	root, err := os.OpenRoot(o.Root())
	if err != nil {
		for a := range want {
			step(a, err)
		}
		return 0
	}
	defer root.Close()
	uidMap := func(uid, gid int) (int, int) { return uid, gid }
	if src := man.Owner; src != nil && (src.UID != o.UID || src.GID != o.GID) {
		m.logf("Files owned by %s (%d:%d) there become %s's (%d:%d) here", src.Name, src.UID, src.GID, o.Name, o.UID, o.GID)
		uidMap = func(uid, gid int) (int, int) {
			if uid == src.UID {
				uid = o.UID
			}
			if gid == src.GID {
				gid = o.GID
			}
			return uid, gid
		}
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		for a := range want {
			step(a, err)
		}
		return 0
	}
	errs := Unpack(f, root, want, uidMap)

	for _, a := range req.Apps {
		if !want[a] {
			continue
		}
		m.setJob(func(j *Job) {
			j.Current = a
			j.Pending = slices.DeleteFunc(j.Pending, func(p string) bool { return p == a })
		})
		if err := errs[a]; err != nil {
			step(a, err)
			continue
		}
		m.logf("%s: restored to %s", a, filepath.Join(o.Root(), a))
		restored++
		if (req.Start || wasRunning[a]) && m.dk != nil {
			m.phase("starting-app")
			name := strings.ToLower(a)
			if s := stackOf(a); s != nil {
				name = s.Name
			}
			m.logf("%s: starting (docker compose up -d)", a)
			out, err := m.dk.StackAction(o, name, "start")
			m.logf("%s", out)
			m.phase("apps")
			if err != nil {
				step(a, fmt.Errorf("starting: %w", err))
				continue
			}
		}
		step(a, nil)
	}
	return restored
}
