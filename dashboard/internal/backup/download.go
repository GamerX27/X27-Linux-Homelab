package backup

import (
	"archive/tar"
	"compress/gzip"
	"context"
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

// DownloadName is the archive's file name: <hostname>-docker-config-YYYYMMDD-HHMMSS.tar.gz.
func DownloadName(t time.Time) string {
	return hostname() + "-docker-config-" + t.Format("20060102-150405") + ".tar.gz"
}

// Download streams the config backup to w; see WriteConfig. It can't overlap a backup run.
func (m *Manager) Download(ctx context.Context, w io.Writer, o docker.Owner, data []string, stop bool) error {
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
	return WriteConfig(ctx, w, o, stacks, data, stop, m.dk)
}

// WriteConfig writes a .tar.gz of every stack's top-level config files, with paths relative
// to ~/docker so it unpacks straight back in. The stacks named in data get their whole folder
// instead; with stop, a running one is stopped while it's packed and started again after,
// as backupApp does. Modes, numeric owners and mtimes are kept; symlinks aren't followed.
func WriteConfig(ctx context.Context, w io.Writer, o docker.Owner, stacks []docker.Stack, data []string, stop bool, dk *docker.Client) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	now := time.Now()

	var withData []string
	for _, s := range stacks {
		if slices.Contains(data, s.Name) {
			withData = append(withData, s.Name)
		}
	}
	readme := readmeText(now, withData)
	if err := tw.WriteHeader(&tar.Header{Name: "README.txt", Mode: 0o644, Size: int64(len(readme)), ModTime: now,
		Uid: o.UID, Gid: o.GID, Typeflag: tar.TypeReg}); err != nil {
		return err
	}
	if _, err := io.WriteString(tw, readme); err != nil {
		return err
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

func readmeText(t time.Time, withData []string) string {
	data := "none"
	if len(withData) > 0 {
		data = strings.Join(withData, ", ")
	}
	return fmt.Sprintf(`Docker config backup of %s, taken %s.

Every app in ~/docker with the files at the top of its folder (compose file, .env and so on).
Apps included with their data (the whole folder): %s.
Named Docker volumes aren't included.

To restore, unpack it into ~/docker and start each app again:

  tar -xzf <this file> -C ~/docker
  cd ~/docker/<app> && docker compose up -d
`, hostname(), t.Format("2006-01-02 15:04:05 MST"), data)
}

// writeConfigOnly adds the stack folder and the config files at its top.
func writeConfigOnly(tw *tar.Writer, dir string) error {
	base := filepath.Base(dir)
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
		log.Printf("config backup: stopping %s while its data is copied", s.Name)
		if out, serr := dk.StackAction(o, s.Name, "stop"); serr != nil {
			return fmt.Errorf("stopping: %w %s", serr, strings.TrimSpace(out))
		}
		defer func() {
			log.Printf("config backup: starting %s again", s.Name)
			if out, serr := dk.StackAction(o, s.Name, "resume"); serr != nil {
				log.Printf("config backup: starting %s again: %v %s", s.Name, serr, strings.TrimSpace(out))
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
		return addEntry(tw, p, filepath.ToSlash(rel), info)
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
