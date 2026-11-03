package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
)

// maxConfigFile is the largest top-level file counted as config rather than data.
const maxConfigFile = 1 << 20

// DataApp is a stack whose folder holds more than its top-level config files.
type DataApp struct {
	Name    string `json:"name"`
	Size    int64  `json:"size"` // the data, without the config files
	Running bool   `json:"running"`
}

// isConfigFile is the rule for what the config backup always takes from the top of a stack
// folder: regular files up to 1 MiB (compose.yml, .env, Caddyfile and so on). Folders,
// symlinks and big files are data.
func isConfigFile(info fs.FileInfo) bool {
	return info.Mode().IsRegular() && info.Size() <= maxConfigFile
}

// configFiles lists the config files at the top of dir and whether anything else is there.
func configFiles(dir string) (files []fs.FileInfo, data bool) {
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		if isConfigFile(info) {
			files = append(files, info)
		} else {
			data = true
		}
	}
	return files, data
}

// rootStacks are the stacks with a folder in ~/docker.
func rootStacks(o docker.Owner, cs []docker.Container) []docker.Stack {
	return slices.DeleteFunc(docker.ListStacks(o, cs), func(s docker.Stack) bool { return !s.InRoot })
}

// DataApps lists the stacks in ~/docker that hold data beyond their config files, with its size.
func DataApps(o docker.Owner, cs []docker.Container) []DataApp {
	out := []DataApp{}
	for _, s := range rootStacks(o, cs) {
		cfg, data := configFiles(s.Dir)
		if !data {
			continue
		}
		var size int64
		filepath.WalkDir(s.Dir, func(_ string, d fs.DirEntry, err error) error {
			if err == nil && d.Type().IsRegular() {
				if info, err := d.Info(); err == nil {
					size += info.Size()
				}
			}
			return nil
		})
		for _, f := range cfg {
			size -= f.Size()
		}
		out = append(out, DataApp{Name: s.Name, Size: max(size, 0), Running: s.Running > 0})
	}
	return out
}

// ExportName is the archive's file name: <hostname>-export-YYYYMMDD-HHMMSS.tar.gz.
func ExportName(t time.Time) string {
	return hostname() + "-export-" + t.Format("20060102-150405") + ".tar.gz"
}

// Export streams the export (see WriteExport) with the server parts asked for. It can't
// overlap a backup or import run.
func (m *Manager) Export(ctx context.Context, w io.Writer, o docker.Owner, data []string, stop bool, parts Parts) error {
	cs, err := m.dk.Containers()
	if err != nil {
		return err
	}
	stacks := rootStacks(o, cs)
	for _, a := range data {
		if !docker.ValidStackName(a) || !slices.ContainsFunc(stacks, func(s docker.Stack) bool { return s.Name == a }) {
			return fmt.Errorf("there's no stack %q in %s", a, o.Root())
		}
	}
	if !m.running.TryLock() {
		return ErrRunning
	}
	defer m.running.Unlock()
	man := m.collect(o, parts)
	return WriteExport(ctx, w, o, stacks, data, stop, m.dk, man)
}

// WriteExport writes a .tar.gz with README.txt, server.json (man, with the apps filled in)
// and every stack's top-level config files under docker/, so `tar -xzf … -C ~ docker/`
// puts them back in ~/docker. The stacks named in data get their whole folder instead;
// with stop, a running one is stopped while it's packed and started again after, as
// backupApp does. Modes, numeric owners and mtimes are kept; symlinks aren't followed.
func WriteExport(ctx context.Context, w io.Writer, o docker.Owner, stacks []docker.Stack, data []string, stop bool, dk *docker.Client, man Manifest) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	now := time.Now()

	man.Version, man.Host, man.Taken = ManifestVersion, hostname(), now.Unix()
	man.Owner = &ManifestOwner{Name: o.Name, UID: o.UID, GID: o.GID}
	man.Apps = []ManifestApp{}
	for _, s := range stacks {
		man.Apps = append(man.Apps, ManifestApp{Name: filepath.Base(s.Dir), Data: slices.Contains(data, s.Name)})
	}
	js, err := json.MarshalIndent(man, "", "  ")
	if err != nil {
		return err
	}
	for _, f := range []struct {
		name string
		mode int64
		body []byte
	}{{"README.txt", 0o644, []byte(readmeText(now, man))}, {"server.json", 0o600, append(js, '\n')}} {
		if err := tw.WriteHeader(&tar.Header{Name: f.name, Mode: f.mode, Size: int64(len(f.body)), ModTime: now,
			Uid: o.UID, Gid: o.GID, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if _, err := tw.Write(f.body); err != nil {
			return err
		}
	}

	for _, s := range stacks {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		if slices.Contains(data, s.Name) {
			err = writeDataApp(ctx, tw, o, s, stop, dk)
		} else {
			err = writeConfigOnly(tw, s.Dir)
		}
		if err != nil {
			return fmt.Errorf("%s: %w", s.Name, err)
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func readmeText(t time.Time, man Manifest) string {
	var withData []string
	for _, a := range man.Apps {
		if a.Data {
			withData = append(withData, a.Name)
		}
	}
	data := "none"
	if len(withData) > 0 {
		data = strings.Join(withData, ", ")
	}
	var parts []string
	if man.Dashboard != nil {
		parts = append(parts, "dashboard settings (port, presets, backup schedule, paired nodes, API tokens)")
	}
	if man.Shares != nil {
		parts = append(parts, "network shares")
	}
	if man.Updates != nil {
		parts = append(parts, "automatic updates, Gotify and services")
	}
	if man.Network != nil {
		parts = append(parts, "hostname, time zone and IPv4 settings")
	}
	server := "none"
	if len(parts) > 0 {
		server = strings.Join(parts, "; ")
	}
	return fmt.Sprintf(`Export of %s, taken %s.

docker/       Every app in ~/docker with the files at the top of its folder (compose
              file, .env and so on). Named Docker volumes aren't included.
              Apps with their data (the whole folder): %s.
server.json   The server's own settings: %s.

THIS FILE HOLDS SECRETS: the apps' .env files, and in server.json the SMB passwords,
the Gotify token and the paired nodes' tokens. Keep it somewhere safe.

To restore everything, open the dashboard on the new server and use
Backup → Export & import → Import.

To restore the apps by hand, unpack docker/ into your home folder and start each app:

  tar -xzf <this file> -C ~ docker/
  cd ~/docker/<app> && docker compose up -d
`, hostname(), t.Format("2006-01-02 15:04:05 MST"), data, server)
}

// writeConfigOnly adds the stack folder and the config files at its top.
func writeConfigOnly(tw *tar.Writer, dir string) error {
	base := "docker/" + filepath.Base(dir)
	info, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if err := addEntry(tw, dir, base, info); err != nil {
		return err
	}
	files, _ := configFiles(dir)
	for _, f := range files {
		if err := addEntry(tw, filepath.Join(dir, f.Name()), base+"/"+f.Name(), f); err != nil {
			return err
		}
	}
	return nil
}

// writeDataApp adds the whole stack folder, stopping the app around it when asked.
func writeDataApp(ctx context.Context, tw *tar.Writer, o docker.Owner, s docker.Stack, stop bool, dk *docker.Client) (err error) {
	if stop && s.Running > 0 && dk != nil {
		log.Printf("export: stopping %s while its data is copied", s.Name)
		if out, serr := dk.StackAction(o, s.Name, "stop"); serr != nil {
			return fmt.Errorf("stopping: %w %s", serr, strings.TrimSpace(out))
		}
		defer func() {
			log.Printf("export: starting %s again", s.Name)
			if out, serr := dk.StackAction(o, s.Name, "resume"); serr != nil {
				log.Printf("export: starting %s again: %v %s", s.Name, serr, strings.TrimSpace(out))
				if err == nil {
					err = fmt.Errorf("starting it again: %w", serr)
				}
			}
		}()
	}
	parent := filepath.Dir(s.Dir)
	return filepath.WalkDir(s.Dir, func(p string, d fs.DirEntry, err error) error {
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
		rel, _ := filepath.Rel(parent, p)
		return addEntry(tw, p, "docker/"+filepath.ToSlash(rel), info)
	})
}

// addEntry writes one folder, file or symlink; sockets, pipes and devices are skipped.
func addEntry(tw *tar.Writer, p, name string, info fs.FileInfo) error {
	link := ""
	switch {
	case info.Mode()&fs.ModeSymlink != 0:
		l, err := os.Readlink(p)
		if err != nil {
			return err
		}
		link = l
	case info.IsDir(), info.Mode().IsRegular():
	default:
		return nil
	}
	hdr, err := tar.FileInfoHeader(info, link)
	if err != nil {
		return err
	}
	hdr.Name = name
	if info.IsDir() {
		hdr.Name += "/"
	}
	hdr.Uname, hdr.Gname = "", "" // numeric owners, as the scheduled backups keep them
	hdr.AccessTime, hdr.ChangeTime = time.Time{}, time.Time{}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return nil
	}
	f, err := os.Open(p)
	if err != nil {
		return err
	}
	defer f.Close()
	// A file that shrank since it was listed (an app still running) is padded with zeros,
	// so the archive stays readable.
	n, err := io.Copy(tw, io.LimitReader(f, hdr.Size))
	if err != nil {
		return err
	}
	if n < hdr.Size {
		_, err = io.CopyN(tw, zeros{}, hdr.Size-n)
	}
	return err
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}
