// Package storage is the Storage page: NFS and SMB shares mounted on the node. Each mount
// is a systemd .mount unit the dashboard writes (plus an .automount for mount-on-access),
// marked as its own; the units are the only record, read back on every List. Hand-written
// units and /etc/fstab are never touched.
package storage

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

// Moved in tests.
var (
	unitDir      = "/etc/systemd/system"
	credDir      = "/etc/dashboard/smb"
	mountRoot    = "/var/mnt"
	dockerDropIn = "/etc/systemd/system/docker.service.d/dashboard-mounts.conf"
)

// marker is the first line of every file the dashboard writes; files without it aren't ours.
const marker = "# Managed by dashboard"

// escapePath is `systemd-escape --path`: the unit name systemd expects for a mount point
// ("/var/mnt/nas-media" → `var-mnt-nas\x2dmedia`). A unit with any other name is ignored.
func escapePath(p string) string {
	p = strings.Trim(path.Clean(p), "/")
	if p == "" {
		return "-"
	}
	var b strings.Builder
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch {
		case c == '/':
			b.WriteByte('-')
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == ':', c == '_', c == '.' && i > 0:
			b.WriteByte(c)
		default:
			fmt.Fprintf(&b, `\x%02x`, c)
		}
	}
	return b.String()
}

// names returns the mount point, the .mount and the .automount unit names for a mount.
func names(name string) (where, mountUnit, autoUnit string) {
	where = filepath.Join(mountRoot, name)
	base := escapePath(where)
	return where, base + ".mount", base + ".automount"
}

// readMarked returns a unit file's text if the dashboard wrote it. exists is true for any
// file at that path, ours or not.
func readMarked(p string) (text string, ours, exists bool) {
	b, err := os.ReadFile(p)
	if err != nil {
		return "", false, !errors.Is(err, os.ErrNotExist)
	}
	s := string(b)
	return s, strings.HasPrefix(s, marker+"\n"), true
}

// writeFile replaces p in one step, so a crash never leaves half a unit.
func writeFile(p, text string, mode os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(p), ".dashboard-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.WriteString(text); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), p)
}

// removeMarked deletes p only if the dashboard wrote it.
func removeMarked(p string) error {
	if _, ours, _ := readMarked(p); !ours {
		return nil
	}
	return os.Remove(p)
}

func systemctl(args ...string) error {
	_, err := run.Cmd(time.Minute, "systemctl", args...)
	return err
}

// unitError turns systemd's "Job for … failed, see journalctl" into the reason itself:
// the unit's last few journal lines (mount's own error message).
func unitError(unit string, err error) error {
	if err == nil {
		return nil
	}
	out, _ := run.Cmd(5*time.Second, "journalctl", "-u", unit, "-n", "4", "-o", "cat", "--no-pager")
	var lines []string
	for _, l := range strings.Split(out, "\n") {
		// systemd's own "Mounting …" / "Failed to mount …" lines say nothing new.
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "Mounting ") && !strings.HasPrefix(l, "Failed to mount ") &&
			!strings.HasPrefix(l, "Unmounting ") && !strings.Contains(l, ".mount: ") {
			lines = append(lines, l)
		}
	}
	if len(lines) == 0 {
		return err
	}
	return errors.New(hint(strings.Join(lines, "\n")))
}
