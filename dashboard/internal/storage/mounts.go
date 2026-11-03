package storage

import (
	"errors"
	"fmt"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/systemd"
)

// Mount is one share mounted on the node. The first block is what the UI sends; the rest
// is filled in by List.
type Mount struct {
	Name        string `json:"name"`
	Type        string `json:"type"`    // "nfs" or "smb"
	Server      string `json:"server"`  // hostname or IP
	Path        string `json:"path"`    // NFS: the export ("/export/media"); SMB: the share, maybe with a folder ("media/films")
	Version     string `json:"version"` // protocol version; "" lets client and server agree
	ReadOnly    bool   `json:"readOnly"`
	Automount   bool   `json:"automount"`   // mount on first access, not at boot
	DockerWaits bool   `json:"dockerWaits"` // docker.service starts after this mount
	// SMB only
	Username string `json:"username,omitempty"`
	Domain   string `json:"domain,omitempty"`
	Guest    bool   `json:"guest,omitempty"`
	Owner    string `json:"owner,omitempty"`    // the user who owns the files on this node; "" is root
	FileMode string `json:"fileMode,omitempty"` // octal, like 0664
	DirMode  string `json:"dirMode,omitempty"`

	HasPassword bool   `json:"hasPassword"`
	MountPoint  string `json:"mountPoint"`
	Unit        string `json:"unit"`    // the .mount unit
	Enabled     bool   `json:"enabled"` // mounted at boot (or watched, for automount)
	State       string `json:"state"`   // mounted, waiting, mounting, unmounting, failed, off
	Total       uint64 `json:"total,omitempty"`
	Used        uint64 `json:"used,omitempty"`
}

var (
	nameRe    = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,30}[a-z0-9])?$`)
	hostRe    = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9.-]{0,251}[A-Za-z0-9])?$`)
	nfsPathRe = regexp.MustCompile(`^/([A-Za-z0-9._@+-]+(/[A-Za-z0-9._@+-]+)*)?$`)
	smbPathRe = regexp.MustCompile(`^[A-Za-z0-9._$@+-]+(/[A-Za-z0-9._@+-]+)*$`)
	domainRe  = regexp.MustCompile(`^[A-Za-z0-9.-]{0,64}$`)
	ownerRe   = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	modeRe    = regexp.MustCompile(`^0?[0-7]{3}$`)
)

var versions = map[string][]string{"nfs": {"4.2", "4.1", "4", "3"}, "smb": {"3.1.1", "3.0", "2.1"}}

// One change at a time: Save and Action stop, rewrite and start units.
var mu sync.Mutex

// clean checks what the UI sent and returns it tidied: trimmed, defaults filled in, paths
// without the slashes people add. Every value that ends up in a unit file passes here.
func clean(m Mount) (Mount, error) {
	m.Name = strings.TrimSpace(m.Name)
	m.Type = strings.ToLower(strings.TrimSpace(m.Type))
	m.Server = strings.TrimSpace(m.Server)
	m.Path = strings.TrimSpace(m.Path)
	m.Version = strings.TrimSpace(m.Version)
	m.Domain = strings.TrimSpace(m.Domain)
	m.Owner = strings.TrimSpace(m.Owner)
	if !nameRe.MatchString(m.Name) {
		return m, errors.New("a name is 1-32 lowercase letters, digits and dashes, not starting or ending with a dash")
	}
	if m.Type != "nfs" && m.Type != "smb" {
		return m, errors.New("choose NFS or SMB")
	}
	ip := net.ParseIP(strings.Trim(m.Server, "[]"))
	switch {
	case ip != nil:
		m.Server = ip.String()
		if m.Type == "smb" && ip.To4() == nil {
			return m, errors.New("for SMB over IPv6, use the server's hostname")
		}
	case !hostRe.MatchString(m.Server):
		return m, errors.New("enter the server's hostname or IP address")
	}
	if m.Version != "" && !slices.Contains(versions[m.Type], m.Version) {
		return m, fmt.Errorf("%s version must be one of %s", strings.ToUpper(m.Type), strings.Join(versions[m.Type], ", "))
	}
	if m.Type == "nfs" {
		if !strings.HasPrefix(m.Path, "/") {
			m.Path = "/" + m.Path
		}
		if len(m.Path) > 1 {
			m.Path = strings.TrimRight(m.Path, "/")
		}
		if !nfsPathRe.MatchString(m.Path) || dotSegment(m.Path) {
			return m, errors.New("the export path is a folder on the server, like /export/media (letters, digits and . _ - @ +)")
		}
		m.Username, m.Domain, m.Guest, m.Owner, m.FileMode, m.DirMode = "", "", false, "", "", ""
		return m, nil
	}
	m.Path = strings.Trim(m.Path, "/")
	if !smbPathRe.MatchString(m.Path) || dotSegment(m.Path) {
		return m, errors.New("the share is its name on the server, like media, or media/films for a folder in it")
	}
	if m.Guest {
		m.Username, m.Domain = "", ""
	} else if err := credCheck(m.Username, "x", m.Domain); err != nil {
		return m, err
	}
	if !domainRe.MatchString(m.Domain) {
		return m, errors.New("the domain (workgroup) is letters, digits, dots and dashes")
	}
	if m.Owner != "" && !ownerRe.MatchString(m.Owner) {
		return m, errors.New("pick the user who should own the files")
	}
	for _, p := range []*string{&m.FileMode, &m.DirMode} {
		if *p == "" {
			continue
		}
		if !modeRe.MatchString(*p) {
			return m, errors.New("permissions are three octal digits, like 664")
		}
		*p = "0" + strings.TrimPrefix(*p, "0")
		if len(*p) == 3 {
			*p = "0" + *p
		}
	}
	if m.FileMode == "" {
		m.FileMode = "0664"
	}
	if m.DirMode == "" {
		m.DirMode = "0775"
	}
	return m, nil
}

func dotSegment(p string) bool {
	return slices.ContainsFunc(strings.Split(p, "/"), func(s string) bool { return s == "." || s == ".." })
}

// lookupOwner gives the uid and gid an SMB mount presents its files with. Moved in tests.
var lookupOwner = func(name string) (uid, gid string, err error) {
	if name == "" {
		return "0", "0", nil
	}
	u, err := user.Lookup(name)
	if err != nil {
		return "", "", fmt.Errorf("there's no user %s on this node", name)
	}
	return u.Uid, u.Gid, nil
}

// source is the What= of the unit: server:/export, or //server/share.
func source(m Mount) string {
	if m.Type == "smb" {
		return "//" + m.Server + "/" + m.Path
	}
	if strings.Contains(m.Server, ":") {
		return "[" + m.Server + "]:" + m.Path
	}
	return m.Server + ":" + m.Path
}

func fsType(m Mount) string {
	if m.Type == "smb" {
		return "cifs"
	}
	return "nfs"
}

// fsOptions are the options for mount(8): the same in the unit and in a test mount, apart
// from where the credentials file is.
func fsOptions(m Mount, uid, gid, cred string) string {
	o := []string{"rw"}
	if m.ReadOnly {
		o[0] = "ro"
	}
	if m.Version != "" {
		o = append(o, "vers="+m.Version)
	}
	if m.Type == "smb" {
		if m.Guest {
			o = append(o, "guest")
		} else {
			o = append(o, "credentials="+cred)
		}
		o = append(o, "uid="+uid, "gid="+gid, "file_mode="+m.FileMode, "dir_mode="+m.DirMode, "iocharset=utf8")
	}
	return strings.Join(o, ",")
}

// mountUnit is the .mount unit. Without automount it's pulled in by remote-fs.target with
// Wants=, so a server that's down fails this mount and nothing else: the node still boots.
func mountUnit(m Mount, opts string) string {
	where, _, _ := names(m.Name)
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n# Name: %s\n", marker, m.Name)
	if m.Owner != "" {
		fmt.Fprintf(&b, "# Owner: %s\n", m.Owner)
	}
	fmt.Fprintf(&b, "[Unit]\nDescription=Dashboard mount %s (%s)\nWants=network-online.target\nAfter=network-online.target\n\n", m.Name, source(m))
	fmt.Fprintf(&b, "[Mount]\nWhat=%s\nWhere=%s\nType=%s\nOptions=_netdev,%s\nTimeoutSec=45\n", source(m), where, fsType(m), opts)
	if !m.Automount {
		b.WriteString("\n[Install]\nWantedBy=remote-fs.target\n")
	}
	return b.String()
}

// autoUnit mounts the share the first time something opens the folder, and keeps it.
func autoUnit(m Mount) string {
	where, _, _ := names(m.Name)
	return fmt.Sprintf("%s\n# Name: %s\n[Unit]\nDescription=Dashboard mount %s, on first access\n\n[Automount]\nWhere=%s\n\n[Install]\nWantedBy=remote-fs.target\n",
		marker, m.Name, m.Name, where)
}

// parseUnit reads a mount back out of a unit the dashboard wrote.
func parseUnit(text string) (Mount, bool) {
	var m Mount
	if !strings.HasPrefix(text, marker+"\n") {
		return m, false
	}
	for _, line := range strings.Split(text, "\n") {
		if v, ok := strings.CutPrefix(line, "# Name: "); ok {
			m.Name = v
		} else if v, ok := strings.CutPrefix(line, "# Owner: "); ok {
			m.Owner = v
		} else if v, ok := strings.CutPrefix(line, "Where="); ok {
			m.MountPoint = v
		} else if v, ok := strings.CutPrefix(line, "Type="); ok {
			m.Type = map[string]string{"cifs": "smb", "nfs": "nfs", "nfs4": "nfs"}[v]
		} else if v, ok := strings.CutPrefix(line, "What="); ok {
			if s, ok := strings.CutPrefix(v, "//"); ok {
				m.Server, m.Path, _ = strings.Cut(s, "/")
			} else if i := strings.Index(v, ":/"); i >= 0 {
				m.Server, m.Path = strings.Trim(v[:i], "[]"), v[i+1:]
			}
		} else if v, ok := strings.CutPrefix(line, "Options="); ok {
			for _, o := range strings.Split(v, ",") {
				k, val, _ := strings.Cut(o, "=")
				switch k {
				case "ro":
					m.ReadOnly = true
				case "vers":
					m.Version = val
				case "guest":
					m.Guest = true
				case "file_mode":
					m.FileMode = val
				case "dir_mode":
					m.DirMode = val
				}
			}
		}
	}
	return m, m.Name != "" && m.Type != ""
}

// state is what the UI shows, from the units' ActiveState.
func state(automount bool, mount, auto string) string {
	switch mount {
	case "active":
		return "mounted"
	case "activating":
		return "mounting"
	case "deactivating":
		return "unmounting"
	case "failed":
		return "failed"
	}
	if automount && auto == "active" {
		return "waiting"
	}
	return "off"
}

// usage is the share's size and use. A server that's gone can make statfs block for good,
// so it gets two seconds and is then left behind.
func usage(p string) (total, used uint64) {
	type res struct{ total, used uint64 }
	ch := make(chan res, 1)
	go func() {
		var st syscall.Statfs_t
		if syscall.Statfs(p, &st) == nil {
			bs := uint64(st.Bsize)
			ch <- res{st.Blocks * bs, (st.Blocks - st.Bfree) * bs}
		}
		close(ch)
	}()
	select {
	case r := <-ch:
		return r.total, r.used
	case <-time.After(2 * time.Second):
		return 0, 0
	}
}

// List returns the mounts the dashboard manages, with their state.
func List() []Mount {
	if Fake {
		return fakeList()
	}
	out := []Mount{}
	entries, _ := os.ReadDir(unitDir)
	waits := dockerWaits()
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".mount") {
			continue
		}
		text, ours, _ := readMarked(filepath.Join(unitDir, e.Name()))
		m, ok := parseUnit(text)
		if !ours || !ok {
			continue
		}
		where, unit, auto := names(m.Name)
		if unit != e.Name() || m.MountPoint != where {
			continue
		}
		m.Unit = unit
		_, m.Automount, _ = readMarked(filepath.Join(unitDir, auto))
		m.DockerWaits = slices.Contains(waits, where)
		if m.Type == "smb" && !m.Guest {
			_, err := os.Stat(credPath(m.Name))
			m.HasPassword = err == nil
			m.Username, _, m.Domain = readCred(credPath(m.Name))
		}
		st := systemd.Show(unit, "ActiveState", "UnitFileState")
		at := map[string]string{}
		target := st
		if m.Automount {
			at = systemd.Show(auto, "ActiveState", "UnitFileState")
			target = at
		}
		m.Enabled = target["UnitFileState"] == "enabled"
		m.State = state(m.Automount, st["ActiveState"], at["ActiveState"])
		if st["ActiveState"] == "active" {
			m.Total, m.Used = usage(where)
		}
		out = append(out, m)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

// hint adds what to check to mount's terse errors.
func hint(msg string) string {
	l := strings.ToLower(msg)
	add := ""
	switch {
	case strings.Contains(l, "error(13)") || strings.Contains(l, "permission denied") && strings.Contains(l, "cifs"):
		add = "The username or password is wrong, or that user can't open this share."
	case strings.Contains(l, "access denied by server"):
		add = "The server doesn't export this folder to this node. Check the export's allowed clients on the server."
	case strings.Contains(l, "error(2)") || strings.Contains(l, "no such file or directory"):
		add = "The server has no share or export by that name."
	case strings.Contains(l, "error(112)") || strings.Contains(l, "host is down") || strings.Contains(l, "no route to host") ||
		strings.Contains(l, "timed out") || strings.Contains(l, "connection refused"):
		add = "The server can't be reached from this node. Check the address and that the server is on."
	case strings.Contains(l, "error(95)") || strings.Contains(l, "protocol not supported"):
		add = "The server doesn't speak that protocol version. Try another version, or leave it on automatic."
	}
	if add == "" {
		return msg
	}
	return msg + "\n" + add
}

// test mounts the share in a scratch folder under /run, reads its top folder and unmounts
// it again. Only os.Remove is used on the folder, so a failed unmount never deletes files
// on the server.
func test(m Mount, password, uid, gid string) (string, error) {
	dir, err := os.MkdirTemp("/run", "dashboard-mount-test-")
	if err != nil {
		return "", err
	}
	defer os.Remove(dir)
	cred := ""
	if m.Type == "smb" && !m.Guest {
		cred = dir + ".cred"
		text, err := credText(m.Username, password, m.Domain)
		if err != nil {
			return "", err
		}
		if err := writeFile(cred, text, 0o600); err != nil {
			return "", err
		}
		defer os.Remove(cred)
	}
	if out, err := run.Cmd(45*time.Second, "mount", "-t", fsType(m), "-o", fsOptions(m, uid, gid, cred), source(m), dir); err != nil {
		if out == "" {
			out = err.Error()
		}
		return "", errors.New(hint(out))
	}
	n := -1
	ch := make(chan int, 1)
	go func() {
		e, err := os.ReadDir(dir)
		if err != nil {
			ch <- -1
			return
		}
		ch <- len(e)
	}()
	select {
	case n = <-ch:
	case <-time.After(5 * time.Second):
	}
	if _, err := run.Cmd(30*time.Second, "umount", dir); err != nil {
		run.Cmd(10*time.Second, "umount", "-l", dir)
	}
	if n < 0 {
		return "", fmt.Errorf("%s mounted, but its contents can't be read", source(m))
	}
	return fmt.Sprintf("Connected to %s: %d item(s) at the top.", source(m), n), nil
}

// password is the one to use: the one typed, or for an edit, the stored one.
func password(m Mount, typed string) (string, error) {
	if m.Type != "smb" || m.Guest {
		return "", nil
	}
	if typed != "" {
		return typed, nil
	}
	if _, pw, _ := readCred(credPath(m.Name)); pw != "" {
		return pw, nil
	}
	return "", errors.New("enter the password")
}

// Test tries a mount without saving anything.
func Test(in Mount, typed string) (string, error) {
	m, err := clean(in)
	if err != nil {
		return "", err
	}
	if Fake {
		return fakeTest(m)
	}
	pw, err := password(m, typed)
	if err != nil {
		return "", err
	}
	uid, gid, err := lookupOwner(m.Owner)
	if err != nil {
		return "", err
	}
	return test(m, pw, uid, gid)
}

// mounted reports whether something is mounted at p.
func mounted(p string) bool {
	b, _ := os.ReadFile("/proc/self/mounts")
	for _, line := range strings.Split(string(b), "\n") {
		if f := strings.Fields(line); len(f) > 1 && f[1] == p {
			return true
		}
	}
	return false
}

// Save creates a mount (create) or changes one. It tests the mount first and keeps
// nothing if that fails; then it writes the units and mounts it.
func Save(in Mount, typed string, create bool) error {
	m, err := clean(in)
	if err != nil {
		return err
	}
	if Fake {
		return fakeSave(m, create)
	}
	mu.Lock()
	defer mu.Unlock()
	where, unit, auto := names(m.Name)
	unitPath, autoPath := filepath.Join(unitDir, unit), filepath.Join(unitDir, auto)
	_, ours, exists := readMarked(unitPath)
	switch {
	case create && exists && ours:
		return fmt.Errorf("there's already a mount named %s", m.Name)
	case create && exists:
		return fmt.Errorf("%s already exists and wasn't made by the dashboard", unitPath)
	case create && mounted(where):
		return fmt.Errorf("something is already mounted at %s", where)
	case !create && !ours:
		return fmt.Errorf("there's no mount named %s", m.Name)
	}
	if create {
		if e, err := os.ReadDir(where); err == nil && len(e) > 0 {
			return fmt.Errorf("%s isn't empty; a share mounted there would hide its files", where)
		}
	}
	pw, err := password(m, typed)
	if err != nil {
		return err
	}
	uid, gid, err := lookupOwner(m.Owner)
	if err != nil {
		return err
	}
	if _, err := test(m, pw, uid, gid); err != nil {
		return err
	}

	if !create {
		if _, wasAuto, _ := readMarked(autoPath); wasAuto {
			systemctl("disable", "--now", auto)
		}
		if err := systemctl("stop", unit); err != nil {
			return fmt.Errorf("couldn't unmount %s to change it (stop the containers that use it first): %v", where, err)
		}
		systemctl("disable", unit)
	}
	if m.Type == "smb" && !m.Guest {
		text, err := credText(m.Username, pw, m.Domain)
		if err != nil {
			return err
		}
		if err := writeCred(m.Name, text); err != nil {
			return err
		}
	} else {
		os.Remove(credPath(m.Name))
	}
	if err := writeFile(unitPath, mountUnit(m, fsOptions(m, uid, gid, credPath(m.Name))), 0o644); err != nil {
		return err
	}
	target := unit
	if m.Automount {
		target = auto
		if err := writeFile(autoPath, autoUnit(m), 0o644); err != nil {
			return err
		}
	} else if err := removeMarked(autoPath); err != nil {
		return err
	}
	if err := os.MkdirAll(where, 0o755); err != nil {
		return err
	}
	if err := setDockerWaits(where, m.DockerWaits); err != nil {
		return err
	}
	if err := systemctl("daemon-reload"); err != nil {
		return err
	}
	return unitError(unit, systemctl("enable", "--now", target))
}

// Action is mount, unmount, enable, disable or remove on a saved mount.
func Action(name, action string) error {
	if !nameRe.MatchString(name) {
		return errors.New("unknown mount")
	}
	if Fake {
		return fakeAction(name, action)
	}
	mu.Lock()
	defer mu.Unlock()
	where, unit, auto := names(name)
	if _, ours, _ := readMarked(filepath.Join(unitDir, unit)); !ours {
		return fmt.Errorf("there's no mount named %s", name)
	}
	_, isAuto, _ := readMarked(filepath.Join(unitDir, auto))
	target := unit
	if isAuto {
		target = auto
	}
	switch action {
	case "mount":
		if isAuto {
			systemctl("start", auto)
		}
		return unitError(unit, systemctl("start", unit))
	case "unmount":
		// The automount goes first, or the next look at the folder mounts it again.
		if isAuto {
			systemctl("stop", auto)
		}
		return unitError(unit, systemctl("stop", unit))
	case "enable":
		return systemctl("enable", target)
	case "disable":
		return systemctl("disable", target)
	case "remove":
		if isAuto {
			systemctl("disable", "--now", auto)
		}
		if err := systemctl("stop", unit); err != nil {
			return fmt.Errorf("couldn't unmount %s (stop the containers that use it first): %v", where, err)
		}
		systemctl("disable", unit)
		for _, p := range []string{filepath.Join(unitDir, auto), filepath.Join(unitDir, unit)} {
			if err := removeMarked(p); err != nil {
				return err
			}
		}
		os.Remove(credPath(name))
		if err := setDockerWaits(where, false); err != nil {
			return err
		}
		os.Remove(where)                // only if empty: never the share's files
		systemctl("reset-failed", unit) // or a mount that had failed stays in systemctl --failed
		return systemctl("daemon-reload")
	}
	return errors.New("unknown action")
}

// Exports lists what an NFS server exports, for the path picker. NFSv4-only servers
// don't run the service this asks.
func Exports(server string) ([]string, error) {
	if net.ParseIP(server) == nil && !hostRe.MatchString(server) {
		return nil, errors.New("enter the server's hostname or IP address")
	}
	if Fake {
		return []string{"/export/media", "/export/backups"}, nil
	}
	out, err := run.Cmd(15*time.Second, "showmount", "-e", "--no-headers", server)
	if err != nil {
		return nil, fmt.Errorf("%s didn't list its exports (%s). NFSv4-only servers don't; type the path instead", server, out)
	}
	return parseExports(out), nil
}

func parseExports(out string) []string {
	list := []string{}
	for _, line := range strings.Split(out, "\n") {
		if f := strings.Fields(line); len(f) > 0 && strings.HasPrefix(f[0], "/") {
			list = append(list, f[0])
		}
	}
	return list
}
