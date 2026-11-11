package files

import (
	"archive/tar"
	"archive/zip"
	"bufio"
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
	"strconv"
	"strings"
	"sync"
)

// Progress reports how far a long operation is: done out of total bytes. total is -1 when
// it isn't known (7z and rar, which an outside tool unpacks).
type Progress func(done, total int64)

func (p Progress) report(done, total int64) {
	if p != nil {
		p(done, total)
	}
}

// countReader counts what's read through it and reports it.
type countReader struct {
	r     io.Reader
	n     int64
	total int64
	prog  Progress
}

func (c *countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	c.prog.report(c.n, c.total)
	return n, err
}

// Paths a running extract or compress would leave half-written. A cancelled fsop (SIGTERM)
// removes them before it exits, since deferred cleanups don't run then.
var (
	cleanupMu    sync.Mutex
	cleanupPaths = map[string]bool{}
)

func addCleanup(p string) {
	cleanupMu.Lock()
	cleanupPaths[p] = true
	cleanupMu.Unlock()
}

func dropCleanup(p string) {
	cleanupMu.Lock()
	delete(cleanupPaths, p)
	cleanupMu.Unlock()
}

// RunCleanups removes everything an interrupted extract or compress left behind.
func RunCleanups() {
	cleanupMu.Lock()
	defer cleanupMu.Unlock()
	for p := range cleanupPaths {
		os.RemoveAll(p)
	}
}

// tmpPrefix marks the dashboard's own temporary files; compressing a folder skips them.
const tmpPrefix = ".dashboard-"

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

func isTar(ext string) bool {
	switch ext {
	case ".tar", ".tar.gz", ".tgz", ".tar.bz2", ".tbz2", ".tbz", ".tar.xz", ".txz", ".tar.zst", ".tzst":
		return true
	}
	return false
}

// splitName splits "notes.txt" into "notes" and ".txt", keeping archive extensions whole;
// folders and dotfiles have no extension.
func splitName(name string, dir bool) (string, string) {
	if dir {
		return name, ""
	}
	if stem, ext := SplitArchive(name); ext != "" {
		return stem, ext
	}
	if i := strings.LastIndex(name, "."); i > 0 {
		return name[:i], name[i:]
	}
	return name, ""
}

// Where Extract puts what it unpacks.
const (
	IntoFolder = "folder" // a new folder next to the archive (the default)
	IntoHere   = "here"   // the archive's own folder
	IntoTo     = "to"     // a folder the user picked
)

// What Extract does when something with the same name is already there (IntoHere, IntoTo).
const (
	KeepBoth  = "keep"      // the new one gets "name (2)" (the default)
	Skip      = "skip"      // keep what's there
	Overwrite = "overwrite" // replace files; a file and a folder of one name are both kept
)

type ExtractOpts struct {
	Path     string // the archive
	Into     string // IntoFolder, IntoHere or IntoTo
	To       string // the folder, for IntoTo
	Name     string // the new folder's name, for IntoFolder; defaults to the archive's
	Conflict string // KeepBoth, Skip or Overwrite
	Progress Progress
}

type ExtractResult struct {
	Path     string `json:"path"` // the new folder or file, or the folder unpacked into
	Skipped  int    `json:"skipped,omitempty"`
	Renamed  int    `json:"renamed,omitempty"`
	Replaced int    `json:"replaced,omitempty"`
}

// Extract unpacks an archive next to itself, into a new folder named after it (or, for
// a plain .gz/.bz2, into the one file it holds). It returns the new path.
func Extract(home, rel string) (string, error) {
	r, err := ExtractWith(home, ExtractOpts{Path: rel})
	return r.Path, err
}

// ExtractWith unpacks an archive as o says. In a folder that already has files, the archive
// is unpacked into a hidden temporary folder first and then moved into place entry by entry,
// so a damaged archive changes nothing and conflicts are handled the same for every format.
func ExtractWith(home string, o ExtractOpts) (ExtractResult, error) {
	var res ExtractResult
	src, err := Resolve(home, o.Path)
	if err != nil {
		return res, err
	}
	st, err := os.Stat(src)
	if err != nil {
		return res, clean(err, home)
	}
	if st.IsDir() {
		return res, errors.New("that's a folder")
	}
	stem, ext := SplitArchive(filepath.Base(src))
	if ext == "" {
		return res, errors.New("that isn't an archive this can extract (zip, tar, tar.gz, tar.bz2, tar.xz, tar.zst, 7z, rar, gz, bz2)")
	}
	switch o.Conflict {
	case "", KeepBoth, Skip, Overwrite:
	default:
		return res, errors.New("unknown choice for files that already exist")
	}
	parent := filepath.Dir(src)
	switch o.Into {
	case "", IntoFolder, IntoHere:
	case IntoTo:
		if parent, err = Resolve(home, o.To); err != nil {
			return res, err
		}
		if st, err := os.Stat(parent); err != nil {
			return res, clean(err, home)
		} else if !st.IsDir() {
			return res, errors.New("the destination isn't a folder")
		}
	default:
		return res, errors.New("unknown place to extract to")
	}
	name := stem
	if o.Name != "" && (o.Into == "" || o.Into == IntoFolder) {
		if err := ValidName(o.Name); err != nil {
			return res, err
		}
		name = o.Name
	}

	if ext == ".gz" || ext == ".bz2" {
		return extractSingle(home, src, parent, name, ext, o)
	}

	if o.Into == "" || o.Into == IntoFolder {
		dst := filepath.Join(parent, freeName(parent, name, ""))
		if err := os.Mkdir(dst, 0o755); err != nil {
			return res, clean(err, home)
		}
		addCleanup(dst)
		defer dropCleanup(dst)
		if err := unpack(src, dst, ext, o.Progress); err != nil {
			os.RemoveAll(dst)
			return res, clean(err, home)
		}
		res.Path = relTo(home, dst)
		return res, nil
	}

	tmp, err := os.MkdirTemp(parent, tmpPrefix+"extract-*")
	if err != nil {
		return res, clean(err, home)
	}
	addCleanup(tmp)
	defer func() { os.RemoveAll(tmp); dropCleanup(tmp) }()
	if err := unpack(src, tmp, ext, o.Progress); err != nil {
		return res, clean(err, home)
	}
	if err := merge(tmp, parent, o.Conflict, &res); err != nil {
		return res, clean(err, home)
	}
	res.Path = relTo(home, parent)
	return res, nil
}

// extractSingle decompresses a plain .gz or .bz2 into the one file it holds.
func extractSingle(home, src, parent, name, ext string, o ExtractOpts) (ExtractResult, error) {
	var res ExtractResult
	tmp, err := os.CreateTemp(parent, tmpPrefix+"extract-*")
	if err != nil {
		return res, clean(err, home)
	}
	tmp.Close()
	addCleanup(tmp.Name())
	defer func() { os.Remove(tmp.Name()); dropCleanup(tmp.Name()) }()
	if err := decompress(src, tmp.Name(), ext, o.Progress); err != nil {
		return res, clean(err, home)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return res, clean(err, home)
	}
	dst := filepath.Join(parent, name)
	if _, err := os.Lstat(dst); err == nil {
		switch o.Conflict {
		case Skip:
			res.Skipped = 1
			res.Path = relTo(home, dst)
			return res, nil
		case Overwrite:
			if st, _ := os.Lstat(dst); st != nil && st.IsDir() {
				dst = filepath.Join(parent, freeFile(parent, name))
				res.Renamed = 1
			} else {
				res.Replaced = 1
			}
		default:
			dst = filepath.Join(parent, freeFile(parent, name))
			if o.Into == IntoHere || o.Into == IntoTo {
				res.Renamed = 1
			}
		}
	}
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return res, clean(err, home)
	}
	res.Path = relTo(home, dst)
	return res, nil
}

// freeFile is freeName for a file name: "x.log" -> "x (2).log".
func freeFile(dir, name string) string {
	stem, ext := splitName(name, false)
	return freeName(dir, stem, ext)
}

// unpack extracts the archive at src into the (empty) folder dst.
func unpack(src, dst, ext string, prog Progress) error {
	switch {
	case ext == ".zip":
		return extractZip(src, dst, prog)
	case isTar(ext):
		return extractTar(src, dst, ext, prog)
	default:
		prog.report(0, -1)
		return extractExternal(src, dst, ext)
	}
}

// merge moves everything in src into dst. Folders that exist in both are merged; for
// anything else already there, conflict decides. It never follows a link in dst.
func merge(src, dst, conflict string, res *ExtractResult) error {
	ents, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range ents {
		s, d := filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())
		dl, err := os.Lstat(d)
		if errors.Is(err, fs.ErrNotExist) {
			if err := os.Rename(s, d); err != nil {
				return err
			}
			continue
		}
		if err != nil {
			return err
		}
		sl, err := os.Lstat(s)
		if err != nil {
			return err
		}
		if sl.IsDir() && dl.IsDir() {
			if err := merge(s, d, conflict, res); err != nil {
				return err
			}
			continue
		}
		switch {
		case conflict == Skip:
			res.Skipped++
			continue
		case conflict == Overwrite && !sl.IsDir() && !dl.IsDir():
			if err := os.Rename(s, d); err != nil {
				return err
			}
			res.Replaced++
			continue
		}
		stem, ext := splitName(e.Name(), sl.IsDir())
		if err := os.Rename(s, filepath.Join(dst, freeName(dst, stem, ext))); err != nil {
			return err
		}
		res.Renamed++
	}
	return nil
}

func decompress(src, dst, ext string, prog Progress) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	st, _ := in.Stat()
	cr := &countReader{r: bufio.NewReader(in), total: st.Size(), prog: prog}
	var r io.Reader
	if ext == ".gz" {
		gz, err := gzip.NewReader(cr)
		if err != nil {
			return errors.New("that isn't a valid gzip file")
		}
		r = gz
	} else {
		r = bzip2.NewReader(cr)
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_TRUNC, 0o644)
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

func newExtractor(root string) *extractor {
	return &extractor{root: root, dirs: map[string]os.FileMode{}}
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

func extractZip(src, dst string, prog Progress) error {
	zr, err := zip.OpenReader(src)
	if err != nil {
		return errors.New("that isn't a valid zip file")
	}
	defer zr.Close()
	// Progress is the unpacked size, which the zip's directory knows up front.
	var total, done int64
	for _, f := range zr.File {
		total += int64(f.UncompressedSize64)
	}
	prog.report(0, total)
	x := newExtractor(dst)
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
				cr := &countReader{r: rc, n: done, total: total, prog: prog}
				err = x.file(f.Name, mode, cr)
				done = cr.n
				rc.Close()
			}
		}
		if err != nil {
			return err
		}
	}
	return x.finish()
}

// tarStream opens a tar archive for reading, decompressed. Progress is how much of the
// archive file has been read. .tar.xz and .tar.zst go through xz or zstd on the node, so
// the tar itself is still read here, with the same checks as every other format.
func tarStream(src, ext string, prog Progress) (io.Reader, func() error, error) {
	f, err := os.Open(src)
	if err != nil {
		return nil, nil, err
	}
	st, _ := f.Stat()
	cr := &countReader{r: f, total: st.Size(), prog: prog}
	prog.report(0, st.Size())
	closeFile := func() error { return f.Close() }
	switch ext {
	case ".tar":
		return cr, closeFile, nil
	case ".tar.gz", ".tgz":
		gz, err := gzip.NewReader(bufio.NewReader(cr))
		if err != nil {
			f.Close()
			return nil, nil, errors.New("that isn't a valid gzip file")
		}
		return gz, closeFile, nil
	case ".tar.bz2", ".tbz2", ".tbz":
		return bzip2.NewReader(bufio.NewReader(cr)), closeFile, nil
	}
	tool := "xz"
	if ext == ".tar.zst" || ext == ".tzst" {
		tool = "zstd"
	}
	bin, err := exec.LookPath(tool)
	if err != nil {
		f.Close()
		return nil, nil, fmt.Errorf("%s files need %s on this node", ext, tool)
	}
	cmd := exec.Command(bin, "-d", "-c", "-q")
	cmd.Stdin = cr
	var stderr strings.Builder
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		f.Close()
		return nil, nil, err
	}
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, nil, err
	}
	return out, func() error {
		io.Copy(io.Discard, out)
		err := cmd.Wait()
		f.Close()
		if err != nil {
			if msg := strings.TrimSpace(stderr.String()); msg != "" {
				return fmt.Errorf("%s: %s", tool, msg)
			}
			return fmt.Errorf("%s: %w", tool, err)
		}
		return nil
	}, nil
}

func extractTar(src, dst, ext string, prog Progress) error {
	r, done, err := tarStream(src, ext, prog)
	if err != nil {
		return err
	}
	err = readTar(r, dst)
	if cerr := done(); err == nil {
		err = cerr
	}
	return err
}

func readTar(r io.Reader, dst string) error {
	tr := tar.NewReader(r)
	x := newExtractor(dst)
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

// extractExternal hands formats Go can't read (7z, rar) to a tool on the node. It runs as
// the user, like everything here, so it can't reach anything they couldn't over SSH.
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
	default:
		return fmt.Errorf("can't extract %s files", ext)
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

// ---- Listing ----------------------------------------------------------------------------

// MaxListed is how many entries ListArchive returns; Count and Size still cover them all.
const MaxListed = 2000

type ArchiveEntry struct {
	Name string `json:"name"`
	Size int64  `json:"size"` // -1 when the tool that lists it doesn't say
	Dir  bool   `json:"dir,omitempty"`
}

type ArchiveList struct {
	Entries   []ArchiveEntry `json:"entries"`
	Count     int            `json:"count"`     // entries in the archive
	Size      int64          `json:"size"`      // unpacked size; -1 if unknown
	Truncated bool           `json:"truncated"` // more than MaxListed
	Top       string         `json:"top"`       // the one folder everything is in, if there is one
	Single    bool           `json:"single"`    // a plain .gz/.bz2: it unpacks to one file
}

// ListArchive lists what's in an archive without unpacking it.
func ListArchive(home, rel string) (ArchiveList, error) {
	l := ArchiveList{Entries: []ArchiveEntry{}}
	src, err := Resolve(home, rel)
	if err != nil {
		return l, err
	}
	st, err := os.Stat(src)
	if err != nil {
		return l, clean(err, home)
	}
	stem, ext := SplitArchive(filepath.Base(src))
	if ext == "" || st.IsDir() {
		return l, errors.New("that isn't an archive")
	}
	add := func(name string, size int64, dir bool) {
		name = strings.TrimLeft(strings.ReplaceAll(name, "\\", "/"), "/")
		if strings.HasSuffix(name, "/") {
			dir, name = true, strings.TrimSuffix(name, "/")
		}
		if name == "" || name == "." {
			return
		}
		l.Count++
		if size > 0 && l.Size >= 0 {
			l.Size += size
		} else if size < 0 {
			l.Size = -1
		}
		if len(l.Entries) < MaxListed {
			l.Entries = append(l.Entries, ArchiveEntry{Name: name, Size: size, Dir: dir})
		} else {
			l.Truncated = true
		}
	}
	switch {
	case ext == ".gz" || ext == ".bz2":
		l.Single = true
		add(stem, -1, false)
	case ext == ".zip":
		zr, err := zip.OpenReader(src)
		if err != nil {
			return l, errors.New("that isn't a valid zip file")
		}
		defer zr.Close()
		for _, f := range zr.File {
			add(f.Name, int64(f.UncompressedSize64), f.Mode().IsDir())
		}
	case isTar(ext):
		r, done, err := tarStream(src, ext, nil)
		if err != nil {
			return l, err
		}
		tr := tar.NewReader(r)
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				done()
				return l, fmt.Errorf("the archive is damaged: %w", err)
			}
			switch hdr.Typeflag {
			case tar.TypeDir:
				add(hdr.Name, 0, true)
			case tar.TypeReg, tar.TypeSymlink, tar.TypeLink:
				add(hdr.Name, hdr.Size, false)
			}
		}
		if err := done(); err != nil {
			return l, err
		}
	default:
		if err := listExternal(src, ext, add); err != nil {
			return l, err
		}
	}
	l.Top = topFolder(l)
	return l, nil
}

// topFolder is the folder every entry is in, when the archive has exactly one at the top.
func topFolder(l ArchiveList) string {
	if l.Truncated || l.Single || len(l.Entries) == 0 {
		return ""
	}
	top := ""
	deeper := false
	for _, e := range l.Entries {
		first, rest, _ := strings.Cut(e.Name, "/")
		if top == "" {
			top = first
		} else if first != top {
			return ""
		}
		if rest != "" || e.Dir {
			deeper = true
		}
	}
	if !deeper {
		return ""
	}
	return top
}

// listExternal lists a 7z or rar with bsdtar (names only) or 7z (names and sizes).
func listExternal(src, ext string, add func(string, int64, bool)) error {
	if bin, err := exec.LookPath("bsdtar"); err == nil {
		out, err := exec.Command(bin, "-tf", src).Output()
		if err != nil {
			return fmt.Errorf("bsdtar couldn't read the archive")
		}
		for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if line != "" {
				add(line, -1, false)
			}
		}
		return nil
	}
	for _, t := range []string{"7z", "7zz", "7za"} {
		bin, err := exec.LookPath(t)
		if err != nil {
			continue
		}
		out, err := exec.Command(bin, "l", "-slt", "-ba", src).Output()
		if err != nil {
			return fmt.Errorf("%s couldn't read the archive", t)
		}
		// Blocks of "Key = value" lines, one block per entry.
		var name string
		var size int64
		var dir bool
		flush := func() {
			if name != "" {
				add(name, size, dir)
			}
			name, size, dir = "", 0, false
		}
		for _, line := range strings.Split(string(out), "\n") {
			k, v, ok := strings.Cut(line, " = ")
			if !ok {
				if strings.TrimSpace(line) == "" {
					flush()
				}
				continue
			}
			switch k {
			case "Path":
				flush()
				name = v
			case "Size":
				size, _ = strconv.ParseInt(v, 10, 64)
			case "Folder":
				dir = v == "+"
			case "Attributes":
				dir = dir || strings.HasPrefix(v, "D")
			}
		}
		flush()
		return nil
	}
	if ext == ".rar" {
		if bin, err := exec.LookPath("unrar"); err == nil {
			out, err := exec.Command(bin, "lb", src).Output()
			if err != nil {
				return errors.New("unrar couldn't read the archive")
			}
			for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
				if line != "" {
					add(line, -1, false)
				}
			}
			return nil
		}
	}
	return fmt.Errorf("listing %s files needs bsdtar or 7z on this node", ext)
}

// ---- Compressing ------------------------------------------------------------------------

// Formats Compress can write, with their extensions.
var CompressFormats = map[string]string{"zip": ".zip", "tar.gz": ".tar.gz", "tar.zst": ".tar.zst"}

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

// walkEntries calls fn for every file, folder and link under the given entries of one
// folder, with its path relative to that folder. What the user can't read is skipped, as
// are the dashboard's own temporary files.
func walkEntries(home string, paths []string, fn func(fp, name string, info os.FileInfo) error) error {
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
			return errors.New("can't compress your whole home folder")
		}
		if _, err := os.Lstat(p); err != nil {
			return clean(err, home)
		}
		roots = append(roots, p)
	}
	for _, root := range roots {
		err := filepath.Walk(root, func(fp string, info os.FileInfo, err error) error {
			if err != nil {
				return nil // skip what the user can't read
			}
			if strings.HasPrefix(info.Name(), tmpPrefix) {
				if info.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			name, _ := filepath.Rel(dir, fp)
			return fn(fp, filepath.ToSlash(name), info)
		})
		if err != nil {
			return err
		}
	}
	return nil
}

// totalSize adds up the regular files under the entries, for progress.
func totalSize(home string, paths []string) int64 {
	var n int64
	walkEntries(home, paths, func(_, _ string, info os.FileInfo) error {
		if info.Mode().IsRegular() {
			n += info.Size()
		}
		return nil
	})
	return n
}

// Zip writes the given entries of one folder, folders with everything in them, to w.
func Zip(home string, paths []string, w io.Writer) error {
	return writeZip(home, paths, w, nil, 0)
}

func writeZip(home string, paths []string, w io.Writer, prog Progress, total int64) error {
	zw := zip.NewWriter(w)
	var done int64
	err := walkEntries(home, paths, func(fp, name string, info os.FileInfo) error {
		hdr, err := zip.FileInfoHeader(info)
		if err != nil {
			return nil
		}
		hdr.Name = name
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
		cr := &countReader{r: f, n: done, total: total, prog: prog}
		_, err = io.Copy(fw, cr)
		done = cr.n
		return err
	})
	if err != nil {
		return err
	}
	return zw.Close()
}

func writeTar(home string, paths []string, w io.Writer, prog Progress, total int64) error {
	tw := tar.NewWriter(w)
	var done int64
	err := walkEntries(home, paths, func(fp, name string, info os.FileInfo) error {
		link := ""
		if info.Mode()&os.ModeSymlink != 0 {
			var err error
			if link, err = os.Readlink(fp); err != nil {
				return nil
			}
		} else if !info.IsDir() && !info.Mode().IsRegular() {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, link)
		if err != nil {
			return nil
		}
		hdr.Name = name
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
		// A file that shrank while being read is padded, so the archive stays valid.
		cr := &countReader{r: f, n: done, total: total, prog: prog}
		n, err := io.Copy(tw, io.LimitReader(cr, hdr.Size))
		done = cr.n
		if err == nil && n < hdr.Size {
			_, err = io.CopyN(tw, zeros{}, hdr.Size-n)
		}
		return err
	})
	if err != nil {
		return err
	}
	return tw.Close()
}

type zeros struct{}

func (zeros) Read(b []byte) (int, error) {
	clear(b)
	return len(b), nil
}

type CompressOpts struct {
	Paths    []string // entries of one folder
	Name     string   // the archive's name without extension; defaults to the entry's, or Archive
	Format   string   // a key of CompressFormats; defaults to zip
	To       string   // the folder to put it in; defaults to the entries' folder
	Progress Progress
}

// Compress zips the entries into a new .zip next to them and returns its path.
func Compress(home string, paths []string) (string, error) {
	return CompressWith(home, CompressOpts{Paths: paths})
}

// CompressWith writes the entries into a new archive. If the name is taken, it gets
// "name (2)". It returns the archive's path.
func CompressWith(home string, o CompressOpts) (string, error) {
	if o.Format == "" {
		o.Format = "zip"
	}
	ext, ok := CompressFormats[o.Format]
	if !ok {
		return "", errors.New("unknown archive format")
	}
	dirRel, err := sameParent(o.Paths)
	if err != nil {
		return "", err
	}
	outRel := dirRel
	if o.To != "" {
		outRel = o.To
	}
	dir, err := Resolve(home, outRel)
	if err != nil {
		return "", err
	}
	if st, err := os.Stat(dir); err != nil {
		return "", clean(err, home)
	} else if !st.IsDir() {
		return "", errors.New("the destination isn't a folder")
	}
	stem := strings.TrimSpace(o.Name)
	if strings.HasSuffix(strings.ToLower(stem), ext) {
		stem = stem[:len(stem)-len(ext)]
	}
	if stem == "" {
		stem = "Archive"
		if len(o.Paths) == 1 {
			stem = path.Base(strings.Trim(o.Paths[0], "/"))
		}
	} else if err := ValidName(stem + ext); err != nil {
		return "", err
	}
	var tool string
	if o.Format == "tar.zst" {
		if tool, err = exec.LookPath("zstd"); err != nil {
			return "", errors.New("tar.zst needs zstd on this node; pick zip or tar.gz")
		}
	}
	total := totalSize(home, o.Paths)
	o.Progress.report(0, total)

	tmp, err := os.CreateTemp(dir, tmpPrefix+"zip-*")
	if err != nil {
		return "", clean(err, home)
	}
	addCleanup(tmp.Name())
	defer func() { os.Remove(tmp.Name()); dropCleanup(tmp.Name()) }()

	switch o.Format {
	case "zip":
		err = writeZip(home, o.Paths, tmp, o.Progress, total)
	case "tar.gz":
		bw := bufio.NewWriterSize(tmp, 1<<16)
		gz := gzip.NewWriter(bw)
		if err = writeTar(home, o.Paths, gz, o.Progress, total); err == nil {
			if err = gz.Close(); err == nil {
				err = bw.Flush()
			}
		}
	case "tar.zst":
		err = zstdTar(tool, home, o, tmp, total)
	}
	if err != nil {
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
	dst := filepath.Join(dir, freeName(dir, stem, ext))
	if err := os.Rename(tmp.Name(), dst); err != nil {
		return "", clean(err, home)
	}
	return relTo(home, dst), nil
}

// zstdTar pipes a tar of the entries through zstd into out.
func zstdTar(tool, home string, o CompressOpts, out *os.File, total int64) error {
	cmd := exec.Command(tool, "-q", "-T0", "-c")
	in, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	cmd.Stdout = out
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	werr := writeTar(home, o.Paths, in, o.Progress, total)
	in.Close()
	if err := cmd.Wait(); err != nil {
		if msg := strings.TrimSpace(stderr.String()); msg != "" {
			return fmt.Errorf("zstd: %s", msg)
		}
		return fmt.Errorf("zstd: %w", err)
	}
	return werr
}
