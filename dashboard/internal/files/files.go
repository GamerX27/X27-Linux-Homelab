// Package files is the Files tab: browsing and changing the logged-in user's home folder.
// The operations here run inside `dashboard fsop`, which the server starts as that user
// (runuser -u <user>), so the kernel applies the user's own permissions. Paths are relative
// to the home folder and can't climb out of it.
package files

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	MaxEdit   = 1 << 20 // text files larger than this aren't opened in the editor
	MaxUpload = 4 << 30
)

var ErrExists = errors.New("a file or folder with that name already exists")

// Resolve turns a path relative to home ("", "docker/jellyfin") into an absolute one,
// refusing absolute paths and anything that climbs above home.
func Resolve(home, rel string) (string, error) {
	rel = strings.ReplaceAll(rel, "\\", "/")
	if strings.HasPrefix(rel, "/") {
		return "", errors.New("paths are relative to your home folder")
	}
	for _, part := range strings.Split(rel, "/") {
		if part == ".." {
			return "", errors.New("paths can't go above your home folder")
		}
	}
	clean := path.Clean("/" + rel)
	abs := filepath.Join(home, filepath.FromSlash(clean))
	if abs != filepath.Clean(home) && !strings.HasPrefix(abs, filepath.Clean(home)+string(filepath.Separator)) {
		return "", errors.New("paths can't go above your home folder")
	}
	return abs, nil
}

// ValidName is a single path element for new files, folders and renames.
func ValidName(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, "/\x00") || len(name) > 255 {
		return errors.New("invalid name")
	}
	return nil
}

type Entry struct {
	Name    string `json:"name"`
	Type    string `json:"type"` // dir, file, link, other
	Size    int64  `json:"size"`
	ModTime int64  `json:"modTime"`
	Mode    string `json:"mode"`
	Target  string `json:"target,omitempty"`  // symlink target
	LinkDir bool   `json:"linkDir,omitempty"` // symlink pointing at a folder
}

func List(home, rel string) ([]Entry, error) {
	dir, err := Resolve(home, rel)
	if err != nil {
		return nil, err
	}
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, clean(err, home)
	}
	out := make([]Entry, 0, len(des))
	for _, de := range des {
		e := Entry{Name: de.Name(), Type: "other"}
		info, err := de.Info()
		if err == nil {
			e.Size, e.ModTime, e.Mode = info.Size(), info.ModTime().Unix(), info.Mode().String()
		}
		switch {
		case de.Type()&fs.ModeSymlink != 0:
			e.Type = "link"
			e.Target, _ = os.Readlink(filepath.Join(dir, de.Name()))
			if st, err := os.Stat(filepath.Join(dir, de.Name())); err == nil && st.IsDir() {
				e.LinkDir = true
			}
		case de.IsDir():
			e.Type = "dir"
		case de.Type().IsRegular():
			e.Type = "file"
		}
		out = append(out, e)
	}
	sort.Slice(out, func(a, b int) bool {
		da, db := out[a].Type == "dir" || out[a].LinkDir, out[b].Type == "dir" || out[b].LinkDir
		if da != db {
			return da
		}
		return strings.ToLower(out[a].Name) < strings.ToLower(out[b].Name)
	})
	return out, nil
}

// clean makes errors read like the shell's, without the absolute path to home.
func clean(err error, home string) error {
	var pe *fs.PathError
	if errors.As(err, &pe) {
		p := strings.TrimPrefix(strings.TrimPrefix(pe.Path, home), "/")
		if p == "" {
			p = "~"
		}
		return fmt.Errorf("%s: %s", p, pe.Err)
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		return le.Err
	}
	return err
}

func IsText(b []byte) bool {
	if !utf8.Valid(b) {
		return false
	}
	for _, c := range b {
		if c == 0 {
			return false
		}
	}
	return true
}

func Read(home, rel string) (string, error) {
	p, err := Resolve(home, rel)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(p)
	if err != nil {
		return "", clean(err, home)
	}
	if st.IsDir() {
		return "", errors.New("that's a folder")
	}
	if st.Size() > MaxEdit {
		return "", errors.New("the file is too large to edit here (over 1 MB); download it instead")
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", clean(err, home)
	}
	if !IsText(b) {
		return "", errors.New("that isn't a text file; download it instead")
	}
	return string(b), nil
}

// Write replaces a file atomically, keeping its permissions. create refuses to overwrite.
func Write(home, rel, content string, create bool) error {
	p, err := Resolve(home, rel)
	if err != nil {
		return err
	}
	if err := ValidName(filepath.Base(p)); err != nil || p == filepath.Clean(home) {
		return errors.New("invalid file name")
	}
	mode := os.FileMode(0o644)
	if st, err := os.Stat(p); err == nil {
		if create {
			return ErrExists
		}
		if st.IsDir() {
			return errors.New("that's a folder")
		}
		mode = st.Mode().Perm()
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".dashboard-edit-*")
	if err != nil {
		return clean(err, home)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return clean(err, home)
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return clean(err, home)
	}
	if err := tmp.Close(); err != nil {
		return clean(err, home)
	}
	return clean(os.Rename(tmp.Name(), p), home)
}

func Mkdir(home, rel string) error {
	p, err := Resolve(home, rel)
	if err != nil {
		return err
	}
	if err := ValidName(filepath.Base(p)); err != nil {
		return err
	}
	if _, err := os.Lstat(p); err == nil {
		return ErrExists
	}
	return clean(os.Mkdir(p, 0o755), home)
}

// Move renames or moves; the target must not exist.
func Move(home, from, to string) error {
	src, err := Resolve(home, from)
	if err != nil {
		return err
	}
	dst, err := Resolve(home, to)
	if err != nil {
		return err
	}
	if src == filepath.Clean(home) || dst == filepath.Clean(home) {
		return errors.New("can't move your home folder")
	}
	if err := ValidName(filepath.Base(dst)); err != nil {
		return err
	}
	if strings.HasPrefix(dst+"/", src+"/") {
		return errors.New("can't move a folder into itself")
	}
	if _, err := os.Lstat(dst); err == nil {
		return ErrExists
	}
	return clean(os.Rename(src, dst), home)
}

// Delete removes a file, link (not its target) or folder with everything in it.
func Delete(home, rel string) error {
	p, err := Resolve(home, rel)
	if err != nil {
		return err
	}
	if p == filepath.Clean(home) {
		return errors.New("can't delete your home folder")
	}
	if _, err := os.Lstat(p); err != nil {
		return clean(err, home)
	}
	return clean(os.RemoveAll(p), home)
}

// Upload streams r into dir/name, through a temp file so a broken upload leaves nothing.
func Upload(home, dirRel, name string, overwrite bool, r io.Reader) (int64, error) {
	if err := ValidName(name); err != nil {
		return 0, err
	}
	dir, err := Resolve(home, dirRel)
	if err != nil {
		return 0, err
	}
	p := filepath.Join(dir, name)
	if st, err := os.Lstat(p); err == nil && (!overwrite || st.IsDir()) {
		return 0, ErrExists
	}
	tmp, err := os.CreateTemp(dir, ".dashboard-upload-*")
	if err != nil {
		return 0, clean(err, home)
	}
	defer os.Remove(tmp.Name())
	n, err := io.Copy(tmp, io.LimitReader(r, MaxUpload+1))
	if err != nil {
		tmp.Close()
		return n, clean(err, home)
	}
	if n > MaxUpload {
		tmp.Close()
		return n, errors.New("the file is larger than 4 GB")
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return n, clean(err, home)
	}
	if err := tmp.Close(); err != nil {
		return n, clean(err, home)
	}
	return n, clean(os.Rename(tmp.Name(), p), home)
}

// Download writes a file as is, or a folder as a .tar.gz, to w. It returns the name the
// browser should save it as.
func Download(home, rel string, w io.Writer) error {
	p, err := Resolve(home, rel)
	if err != nil {
		return err
	}
	st, err := os.Stat(p)
	if err != nil {
		return clean(err, home)
	}
	if !st.IsDir() {
		f, err := os.Open(p)
		if err != nil {
			return clean(err, home)
		}
		defer f.Close()
		_, err = io.Copy(w, f)
		return err
	}
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	base := filepath.Dir(p)
	err = filepath.Walk(p, func(fp string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // skip what the user can't read, like `tar` would with a warning
		}
		name, _ := filepath.Rel(base, fp)
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			link, _ = os.Readlink(fp)
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return nil
		}
		hdr.Name = filepath.ToSlash(name)
		if info.IsDir() {
			hdr.Name += "/"
		}
		if !info.Mode().IsRegular() {
			return tw.WriteHeader(hdr)
		}
		f, err := os.Open(fp)
		if err != nil {
			return nil
		}
		defer f.Close()
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}
