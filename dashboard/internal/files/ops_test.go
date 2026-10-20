package files

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func write(t *testing.T, p, s string, mode os.FileMode) {
	t.Helper()
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, []byte(s), mode); err != nil {
		t.Fatal(err)
	}
}

func TestCopy(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "proj/a.sh"), "#!/bin/sh\n", 0o750)
	write(t, filepath.Join(home, "proj/sub/b.txt"), "b", 0o600)
	os.Symlink("a.sh", filepath.Join(home, "proj/link"))

	if err := Copy(home, "proj", "proj2"); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(home, "proj2/a.sh")); st == nil || st.Mode().Perm() != 0o750 {
		t.Fatalf("mode not kept: %v", st)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "proj2/sub/b.txt")); string(b) != "b" {
		t.Fatalf("content %q", b)
	}
	if l, err := os.Readlink(filepath.Join(home, "proj2/link")); err != nil || l != "a.sh" {
		t.Fatalf("link %q %v", l, err)
	}
	if err := Copy(home, "proj/a.sh", "proj2/a.sh"); !errors.Is(err, ErrExists) {
		t.Fatalf("copy over existing: %v", err)
	}
	if err := Copy(home, "proj", "proj/inner"); err == nil {
		t.Fatal("copied folder into itself")
	}
	if err := Copy(home, "", "x"); err == nil {
		t.Fatal("copied home")
	}
}

func mkzip(t *testing.T, p string, entries map[string]string, links map[string]string) {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	names := make([]string, 0, len(entries))
	for n := range entries {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		w, _ := zw.Create(n)
		io.WriteString(w, entries[n])
	}
	for n, target := range links {
		h := &zip.FileHeader{Name: n}
		h.SetMode(os.ModeSymlink | 0o777)
		w, _ := zw.CreateHeader(h)
		io.WriteString(w, target)
	}
	zw.Close()
	write(t, p, buf.String(), 0o644)
}

func mktgz(t *testing.T, p string, hdrs []tar.Header, bodies []string) {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	for i, h := range hdrs {
		h.Size = int64(len(bodies[i]))
		tw.WriteHeader(&h)
		io.WriteString(tw, bodies[i])
	}
	tw.Close()
	gz.Close()
	write(t, p, buf.String(), 0o644)
}

func TestExtract(t *testing.T) {
	home := t.TempDir()
	mkzip(t, filepath.Join(home, "dl/site.zip"), map[string]string{"index.html": "hi", "css/a.css": "x"},
		map[string]string{"css/ok": "a.css", "evil": "../../etc"})
	p, err := Extract(home, "dl/site.zip")
	if err != nil || p != "dl/site" {
		t.Fatal(p, err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "dl/site/css/a.css")); string(b) != "x" {
		t.Fatalf("content %q", b)
	}
	if _, err := os.Readlink(filepath.Join(home, "dl/site/css/ok")); err != nil {
		t.Fatal("inside link skipped")
	}
	if _, err := os.Lstat(filepath.Join(home, "dl/site/evil")); err == nil {
		t.Fatal("escaping link created")
	}
	if p, err := Extract(home, "dl/site.zip"); err != nil || p != "dl/site (2)" {
		t.Fatal("collision", p, err)
	}

	mktgz(t, filepath.Join(home, "app.tar.gz"),
		[]tar.Header{{Name: "app/", Typeflag: tar.TypeDir, Mode: 0o755}, {Name: "app/run", Typeflag: tar.TypeReg, Mode: 0o4755}},
		[]string{"", "#!/bin/sh"})
	if p, err := Extract(home, "app.tar.gz"); err != nil || p != "app" {
		t.Fatal(p, err)
	}
	if st, _ := os.Stat(filepath.Join(home, "app/app/run")); st == nil || st.Mode() != 0o755 {
		t.Fatalf("mode %v", st)
	}

	var gzbuf bytes.Buffer
	gz := gzip.NewWriter(&gzbuf)
	io.WriteString(gz, "log line\n")
	gz.Close()
	write(t, filepath.Join(home, "x.log.gz"), gzbuf.String(), 0o644)
	if p, err := Extract(home, "x.log.gz"); err != nil || p != "x.log" {
		t.Fatal(p, err)
	}

	// Unsafe archives fail and leave nothing behind.
	mkzip(t, filepath.Join(home, "slip.zip"), map[string]string{"ok.txt": "1", "../evil.txt": "x"}, nil)
	if _, err := Extract(home, "slip.zip"); err == nil {
		t.Fatal("zip slip accepted")
	}
	mktgz(t, filepath.Join(home, "through.tgz"),
		[]tar.Header{{Name: "out", Typeflag: tar.TypeSymlink, Linkname: "."}, {Name: "out/x", Typeflag: tar.TypeReg, Mode: 0o644}},
		[]string{"", "x"})
	if _, err := Extract(home, "through.tgz"); err == nil {
		t.Fatal("wrote through a link")
	}
	for _, left := range []string{"slip", "evil.txt", "through"} {
		if _, err := os.Lstat(filepath.Join(home, left)); err == nil {
			t.Fatalf("%s left behind", left)
		}
	}
	if _, err := Extract(home, "x.log"); err == nil {
		t.Fatal("extracted a non-archive")
	}
}

func TestZipAndCompress(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "p/a.txt"), "a", 0o644)
	write(t, filepath.Join(home, "p/d/b.txt"), "b", 0o644)
	var buf bytes.Buffer
	if err := Zip(home, []string{"p/a.txt", "p/d"}, &buf); err != nil {
		t.Fatal(err)
	}
	zr, _ := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "a.txt,d/,d/b.txt" {
		t.Fatalf("zip %v", names)
	}
	if err := Zip(home, []string{"p/a.txt", "other"}, io.Discard); err == nil {
		t.Fatal("zipped entries from different folders")
	}
	if p, err := Compress(home, []string{"p/d"}); err != nil || p != "p/d.zip" {
		t.Fatal(p, err)
	}
	if p, err := Compress(home, []string{"p/d"}); err != nil || p != "p/d (2).zip" {
		t.Fatal(p, err)
	}
	if p, err := Compress(home, []string{"p/a.txt", "p/d"}); err != nil || p != "p/Archive.zip" {
		t.Fatal(p, err)
	}
}

func TestChmod(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "d/plain"), "", 0o600)
	write(t, filepath.Join(home, "d/exe"), "", 0o700)
	outside := filepath.Join(t.TempDir(), "outside")
	write(t, outside, "", 0o600)
	os.Symlink(outside, filepath.Join(home, "d/link"))

	if err := Chmod(home, "d/plain", 0o644, false); err != nil {
		t.Fatal(err)
	}
	if err := Chmod(home, "d", 0o755, true); err != nil {
		t.Fatal(err)
	}
	for name, want := range map[string]os.FileMode{"d": 0o755, "d/plain": 0o644, "d/exe": 0o755} {
		if st, _ := os.Stat(filepath.Join(home, name)); st.Mode().Perm() != want {
			t.Errorf("%s: %v, want %v", name, st.Mode().Perm(), want)
		}
	}
	if st, _ := os.Stat(outside); st.Mode().Perm() != 0o600 {
		t.Fatal("followed a link out of home")
	}
	if err := Chmod(home, "", 0o777, false); err == nil {
		t.Fatal("changed home")
	}
}

func TestUploadParents(t *testing.T) {
	home := t.TempDir()
	if _, err := Upload(home, "drop/a/b", "f.txt", false, false, strings.NewReader("x")); err == nil {
		t.Fatal("uploaded into a missing folder")
	}
	if _, err := Upload(home, "drop/a/b", "f.txt", false, true, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(home, "drop/a/b/f.txt")); string(b) != "x" {
		t.Fatalf("content %q", b)
	}
}
