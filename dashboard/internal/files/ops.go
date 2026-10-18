package files

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
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
	return filepath.ToSlash(r)
}

// ArchiveExts are the archive types Extract understands, longest first so ".tar.gz"
// wins over ".gz".
var ArchiveExts = []string{".tar.gz", ".tar.bz2", ".tar.xz", ".tar.zst", ".tgz", ".tbz2", ".tbz", ".txz", ".tzst",
	".tar", ".zip", ".7z", ".rar", ".gz", ".bz2"}

// SplitArchive splits "foo.tar.gz" into "foo" and ".tar.gz"; ext is "" for other files.
func SplitArchive(name string) (stem, ext string) {
	lower := strings.ToLower(name)
	for _, e := range ArchiveExts {
		if strings.HasSuffix(lower, e) && len(name) > len(e) {
			return name[:len(name)-len(e)], e
		}
	}
	return name, ""
}

// Extract unpacks an archive next to itself, into a new folder named after it (or, for
// a plain .gz/.bz2, into the one file it holds). It returns the new path.
func Extract(home, rel string) (string, error) {
	src, err := Resolve(home, rel)
	if err != nil {
		return "", err
	}
	st, err := os.Stat(src)
	if err != nil {
		return "", clean(err, home)
	}
	if st.IsDir() {
		return "", errors.New("that's a folder")
	}
	stem, ext := SplitArchive(filepath.Base(src))
	if ext == "" {
		return "", errors.New("that isn't an archive this can extract (zip, tar, tar.gz, tar.bz2, tar.xz, tar.zst, 7z, rar, gz, bz2)")
	}
	parent := filepath.Dir(src)

	if ext == ".gz" || ext == ".bz2" {
		dst := filepath.Join(parent, freeName(parent, stem, ""))
		if err := decompress(src, dst, ext); err != nil {
			os.Remove(dst)
			return "", clean(err, home)
		}
		return relTo(home, dst), nil
	}

	dst := filepath.Join(parent, freeName(parent, stem, ""))
	if err := os.Mkdir(dst, 0o755); err != nil {
		return "", clean(err, home)
	}
	switch ext {
	case ".zip":
		err = extractZip(src, dst)
	case ".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2", ".tbz":
		err = extractTar(src, dst, ext)
	default:
		err = extractExternal(src, dst, ext)
	}
	if err != nil {
		os.RemoveAll(dst)
		return "", clean(err, home)
	}
	return relTo(home, dst), nil
}

func decompress(src, dst, ext string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	var r io.Reader
	if ext == ".gz" {
		gz, err := gzip.NewReader(in)
		if err != nil {
			return err
		}
		r = gz
	} else {
		r = bzip2.NewReader(in)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(r, MaxUpload+1))
	if err == nil && n > MaxUpload {
		err = errTooBig
	}
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	return err
}

var errTooBig = errors.New("the archive unpacks to more than 4 GB")

// extractor writes archive entries under root and nowhere else: no "..", no writing
// through links, no links that point outside, no setuid bits, and at most MaxUpload bytes.
type extractor struct {
	root  string
	total int64
	dirs  map[string]os.FileMode
}

func (x *extractor) target(name string) (string, error) {
	name = strings.TrimLeft(strings.ReplaceAll(name, "\\", "/"), "/")
	p, err := Resolve(x.root, name)
	if err != nil {
		return "", fmt.Errorf("the archive has an unsafe path: %s", name)
	}
	if p == x.root {
		return "", nil
	}
	cur := x.root
	rel, _ := filepath.Rel(x.root, filepath.Dir(p))
	if rel != "." {
		for _, part := range strings.Split(rel, string(filepath.Separator)) {
			cur = filepath.Join(cur, part)
			if st, err := os.Lstat(cur); err == nil && st.Mode()&os.ModeSymlink != 0 {
				return "", fmt.Errorf("the archive writes through a link: %s", name)
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return "", err
	}
	// A later entry with the same name replaces the earlier one, but never through a link.
	if st, err := os.Lstat(p); err == nil && !st.IsDir() {
		if err := os.Remove(p); err != nil {
			return "", err
		}
	}
	return p, nil
}

func (x *extractor) dir(name string, mode os.FileMode) error {
	p, err := x.target(name)
	if err != nil || p == "" {
		return err
	}
	if err := os.MkdirAll(p, 0o755); err != nil {
		return err
	}
	x.dirs[p] = mode
	return nil
}

func (x *extractor) file(name string, mode os.FileMode, r io.Reader) error {
	p, err := x.target(name)
	if err != nil || p == "" {
		return err
	}
	perm := mode.Perm()
	if perm == 0 {
		perm = 0o644
	}
	f, err := os.OpenFile(p, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm|0o200)
	if err != nil {
		return err
	}
	n, err := io.Copy(f, io.LimitReader(r, MaxUpload-x.total+1))
	x.total += n
	if err == nil && x.total > MaxUpload {
		err = errTooBig
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil && perm&0o200 == 0 {
		err = os.Chmod(p, perm)
	}
	return err
}

// inside reports whether a link at p pointing to target stays under root.
func (x *extractor) inside(p, target string) bool {
	if filepath.IsAbs(target) {
		return false
	}
	t := filepath.Join(filepath.Dir(p), target)
	return t == x.root || strings.HasPrefix(t, x.root+string(filepath.Separator))
}

// symlink creates a link if it stays inside the folder; others are skipped.
func (x *extractor) symlink(name, target string) error {
	p, err := x.target(name)
	if err != nil || p == "" || !x.inside(p, target) {
		return err
	}
	return os.Symlink(target, p)
}

// hardlink links name to an earlier entry; it's skipped if that isn't a regular file
// already extracted here.
func (x *extractor) hardlink(name, to string) error {
	src, err := Resolve(x.root, strings.TrimLeft(to, "/"))
	if err != nil {
		return nil
	}
	if st, err := os.Lstat(src); err != nil || !st.Mode().IsRegular() {
		return nil
	}
	p, err := x.target(name)
	if err != nil || p == "" {
		return err
	}
	return os.Link(src, p)
}

// finish applies folder modes last, so read-only folders don't block their contents.
func (x *extractor) finish() error {
	for p, mode := range x.dirs {
		if err := os.Chmod(p, mode.Perm()|0o700); err != nil {
			return err
		}
	}
	return nil
}

func extractZip(src, dst string) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return errors.New("that isn't a valid zip file")
	}
	defer zr.Close()
	x := &extractor{root: dst, dirs: map[string]os.FileMode{}}
	for _, f := range zr.File {
		mode := f.Mode()
		var err error
		switch {
		case mode.IsDir() || strings.HasSuffix(f.Name, "/"):
			perm := mode.Perm()
			if perm == 0 {
				perm = 0o755
			}
			err = x.dir(f.Name, perm)
		case mode&os.ModeSymlink != 0:
			var rc io.ReadCloser
			if rc, err = f.Open(); err == nil {
				var b []byte
				b, err = io.ReadAll(io.LimitReader(rc, 4096))
				rc.Close()
				if err == nil {
					err = x.symlink(f.Name, string(b))
				}
			}
		case mode.IsRegular():
			var rc io.ReadCloser
			if rc, err = f.Open(); err == nil {
				err = x.file(f.Name, mode, rc)
				rc.Close()
			}
		}
		if err != nil {
			return err
		}
	}
	return x.finish()
}

func extractTar(src, dst, ext string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	var r io.Reader = f
	switch ext {
	case ".tar.gz", ".tgz":
		gz, err := gzip.NewReader(f)
		if err != nil {
			return errors.New("that isn't a valid gzip file")
		}
		r = gz
	case ".tar.bz2", ".tbz2", ".tbz":
		r = bzip2.NewReader(f)
	}
	tr := tar.NewReader(r)
	x := &extractor{root: dst, dirs: map[string]os.FileMode{}}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("the archive is damaged: %w", err)
		}
		mode := os.FileMode(hdr.Mode).Perm()
		switch hdr.Typeflag {
		case tar.TypeDir:
			err = x.dir(hdr.Name, mode)
		case tar.TypeReg:
			err = x.file(hdr.Name, mode, tr)
		case tar.TypeSymlink:
			err = x.symlink(hdr.Name, hdr.Linkname)
		case tar.TypeLink:
			err = x.hardlink(hdr.Name, hdr.Linkname)
		}
		if err != nil {
			return err
		}
	}
	return x.finish()
}

// extractExternal hands formats Go can't read to a tool on the node. It runs as the
// user, like everything here, so it can't reach anything they couldn't over SSH.
func extractExternal(src, dst, ext string) error {
	type tool struct {
		bin  string
		args []string
	}
	var tools []tool
	switch ext {
	case ".7z":
		tools = []tool{{"bsdtar", []string{"-xf", src, "-C", dst}}, {"7z", []string{"x", "-y", "-o" + dst, src}},
			{"7zz", []string{"x", "-y", "-o" + dst, src}}, {"7za", []string{"x", "-y", "-o" + dst, src}}}
	case ".rar":
		tools = []tool{{"bsdtar", []string{"-xf", src, "-C", dst}}, {"unrar", []string{"x", "-y", src, dst + "/"}},
			{"7z", []string{"x", "-y", "-o" + dst, src}}, {"7zz", []string{"x", "-y", "-o" + dst, src}}}
	default: // .tar.xz, .tar.zst: GNU tar calls xz/zstd itself
		tools = []tool{{"tar", []string{"-xf", src, "-C", dst, "--no-same-owner"}}, {"bsdtar", []string{"-xf", src, "-C", dst}}}
	}
	for _, t := range tools {
		bin, err := exec.LookPath(t.bin)
		if err != nil {
			continue
		}
		out, err := exec.Command(bin, t.args...).CombinedOutput()
		if err != nil {
			msg := strings.TrimSpace(string(out))
			if msg == "" {
				msg = err.Error()
			}
			return fmt.Errorf("%s: %s", t.bin, msg)
		}
		return nil
	}
	return fmt.Errorf("extracting %s files needs %s on this node", ext, tools[0].bin)
}

// sameParent checks that paths are entries of one folder and returns it.
func sameParent(paths []string) (string, error) {
	if len(paths) == 0 {
		return "", errors.New("nothing selected")
	}
	dir := path.Dir(strings.Trim(paths[0], "/"))
	for _, p := range paths[1:] {
		if path.Dir(strings.Trim(p, "/")) != dir {
			return "", errors.New("the files must be in the same folder")
		}
	}
	if dir == "." {
		dir = ""
	}
	return dir, nil
}

// Zip writes the given entries of one folder, folders with everything in them, to w.
func Zip(home string, paths []string, w io.Writer) error {
	dirRel, err := sameParent(paths)
	if err != nil {
		return err
	}
	dir, err := Resolve(home, dirRel)
	if err != nil {
		return err
	}
	var roots []string
	for _, rel := range paths {
		p, err := Resolve(home, rel)
		if err != nil {
			return err
		}
		if p == filepath.Clean(home) {
			return errors.New("can't zip your whole home folder")
		}
		if _, err := os.Lstat(p); err != nil {
			return clean(err, home)
		}
		roots = append(roots, p)
	}
	zw := zip.NewWriter(w)
	for _, root := range roots {
		err := filepath.Walk(root, func(fp string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // skip what the user can't read
			}
			if strings.HasPrefix(info.Name(), ".dashboard-zip-") {
				return nil
			}
			hdr, err := zip.FileInfoHeader(info)
			if err != nil {
				return nil
			}
			name, _ := filepath.Rel(dir, fp)
			hdr.Name = filepath.ToSlash(name)
			switch {
			case info.IsDir():
				hdr.Name += "/"
				_, err := zw.CreateHeader(hdr)
				return err
			case info.Mode()&os.ModeSymlink != 0:
				target, err := os.Readlink(fp)
				if err != nil {
					return nil
				}
				fw, err := zw.CreateHeader(hdr)
				if err != nil {
					return err
				}
				_, err = io.WriteString(fw, target)
				return err
			case !info.Mode().IsRegular():
				return nil
			}
			f, err := os.Open(fp)
			if err != nil {
				return nil
			}
			defer f.Close()
			hdr.Method = zip.Deflate
			fw, err := zw.CreateHeader(hdr)
			if err != nil {
				return err
			}
			_, err = io.Copy(fw, f)
			return err
		})
		if err != nil {
			return err
		}
	}
	return zw.Close()
}

// Compress zips the entries into a new .zip next to them and returns its path.
func Compress(home string, paths []string) (string, error) {
	dirRel, err := sameParent(paths)
	if err != nil {
		return "", err
	}
	dir, err := Resolve(home, dirRel)
	if err != nil {
		return "", err
	}
	stem := "Archive"
	if len(paths) == 1 {
		stem = path.Base(strings.Trim(paths[0], "/"))
	}
	tmp, err := os.CreateTemp(dir, ".dashboard-zip-*")
	if err != nil {
		return "", clean(err, home)
	}
	defer os.Remove(tmp.Name())
	if err := Zip(home, paths, tmp); err != nil {
		tmp.Close()
		return "", err
	}
	if err := tmp.Chmod(0o644); err != nil {
		tmp.Close()
		return "", clean(err, home)
	}
	if err := tmp.Close(); err != nil {
		return "", clean(err, home)
	}
	dst := filepath.Join(dir, freeName(dir, stem, ".zip"))
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", clean(err, home)
	}
	return relTo(home, dst), nil
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
