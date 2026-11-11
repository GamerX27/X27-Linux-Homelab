package files

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Copy copies a file, link (as a link) or folder with everything in it, keeping modes.
// The target must not exist.
func Copy(home, from, to string) error {
	src, err := Resolve(home, from)
	if err != nil {
		return err
	}
	dst, err := Resolve(home, to)
	if err != nil {
		return err
	}
	if src == filepath.Clean(home) || dst == filepath.Clean(home) {
		return errors.New("can't copy your home folder")
	}
	if err := ValidName(filepath.Base(dst)); err != nil {
		return err
	}
	if strings.HasPrefix(dst+"/", src+"/") {
		return errors.New("can't copy a folder into itself")
	}
	if _, err := os.Lstat(src); err != nil {
		return clean(err, home)
	}
	if _, err := os.Lstat(dst); err == nil {
		return ErrExists
	}
	if err := copyTree(src, dst); err != nil {
		os.RemoveAll(dst)
		return clean(err, home)
	}
	return nil
}

func copyTree(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	switch {
	case info.Mode()&os.ModeSymlink != 0:
		target, err := os.Readlink(src)
		if err != nil {
			return err
		}
		return os.Symlink(target, dst)
	case info.IsDir():
		if err := os.Mkdir(dst, 0o700); err != nil {
			return err
		}
		des, err := os.ReadDir(src)
		if err != nil {
			return err
		}
		for _, de := range des {
			if err := copyTree(filepath.Join(src, de.Name()), filepath.Join(dst, de.Name())); err != nil {
				return err
			}
		}
		return os.Chmod(dst, info.Mode().Perm())
	case info.Mode().IsRegular():
		in, err := os.Open(src)
		if err != nil {
			return err
		}
		defer in.Close()
		out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, info.Mode().Perm())
		if err != nil {
			return err
		}
		if _, err := io.Copy(out, in); err != nil {
			out.Close()
			return err
		}
		return out.Close()
	}
	return nil // sockets, fifos and devices aren't copied
}

// freeName returns stem+ext, or "stem (2)ext", "stem (3)ext"… if that's taken in dir.
func freeName(dir, stem, ext string) string {
	name := stem + ext
	for i := 2; ; i++ {
		if _, err := os.Lstat(filepath.Join(dir, name)); errors.Is(err, fs.ErrNotExist) {
			return name
		}
		name = fmt.Sprintf("%s (%d)%s", stem, i, ext)
	}
}

func relTo(home, abs string) string {
	r, _ := filepath.Rel(filepath.Clean(home), abs)
	if r == "." {
		return "" // the top, as the page names it
	}
	return filepath.ToSlash(r)
}

// Chmod sets permissions. Recursively, folders get mode as is and files get it without
// execute bits, unless they were already executable (like chmod's X). Links are skipped.
func Chmod(home, rel string, mode os.FileMode, recursive bool) error {
	p, err := Resolve(home, rel)
	if err != nil {
		return err
	}
	if p == filepath.Clean(home) {
		return errors.New("can't change your home folder's permissions here")
	}
	mode &= 0o777
	st, err := os.Lstat(p)
	if err != nil {
		return clean(err, home)
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return errors.New("links have no permissions of their own")
	}
	if !recursive || !st.IsDir() {
		return clean(os.Chmod(p, mode), home)
	}
	return clean(filepath.WalkDir(p, func(fp string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		switch {
		case d.Type()&fs.ModeSymlink != 0:
			return nil
		case d.IsDir():
			return os.Chmod(fp, mode)
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Mode().Perm()&0o111 != 0 {
			return os.Chmod(fp, mode)
		}
		return os.Chmod(fp, mode&^0o111)
	}), home)
}
