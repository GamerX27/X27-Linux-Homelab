package files

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolve(t *testing.T) {
	home := "/home/alice"
	for in, want := range map[string]string{"": home, "docker": home + "/docker", "a/./b/": home + "/a/b", "a//b": home + "/a/b"} {
		if got, err := Resolve(home, in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	for _, bad := range []string{"..", "../bob", "a/../../x", "/etc/passwd", "a/..", `..\x`} {
		if _, err := Resolve(home, bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}

func TestOps(t *testing.T) {
	home := t.TempDir()
	if err := Mkdir(home, "proj"); err != nil {
		t.Fatal(err)
	}
	if err := Mkdir(home, "proj"); !errors.Is(err, ErrExists) {
		t.Fatalf("mkdir twice: %v", err)
	}
	if err := Write(home, "proj/a.txt", "hello\n", true); err != nil {
		t.Fatal(err)
	}
	if err := Write(home, "proj/a.txt", "x", true); !errors.Is(err, ErrExists) {
		t.Fatalf("create over existing: %v", err)
	}
	os.Chmod(filepath.Join(home, "proj/a.txt"), 0o600)
	if err := Write(home, "proj/a.txt", "changed\n", false); err != nil {
		t.Fatal(err)
	}
	if st, _ := os.Stat(filepath.Join(home, "proj/a.txt")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode not kept: %v", st.Mode())
	}
	if s, err := Read(home, "proj/a.txt"); err != nil || s != "changed\n" {
		t.Fatalf("read %q %v", s, err)
	}
	os.WriteFile(filepath.Join(home, "proj/bin"), []byte{0x7f, 'E', 'L', 'F', 0, 1}, 0o644)
	if _, err := Read(home, "proj/bin"); err == nil {
		t.Fatal("binary opened as text")
	}
	os.Symlink("/etc", filepath.Join(home, "etc-link"))
	l, err := List(home, "")
	if err != nil || len(l) != 2 || l[0].Name != "etc-link" || !l[0].LinkDir || l[1].Name != "proj" || l[1].Type != "dir" {
		t.Fatalf("list %+v %v", l, err)
	}
	if n, err := Upload(home, "proj", "up.bin", false, strings.NewReader("12345")); err != nil || n != 5 {
		t.Fatal(n, err)
	}
	if _, err := Upload(home, "proj", "up.bin", false, strings.NewReader("x")); !errors.Is(err, ErrExists) {
		t.Fatalf("upload over existing: %v", err)
	}
	if _, err := Upload(home, "proj", "up.bin", true, strings.NewReader("x")); err != nil {
		t.Fatal(err)
	}
	if _, err := Upload(home, "proj", "../escape", false, strings.NewReader("x")); err == nil {
		t.Fatal("bad upload name accepted")
	}
	if err := Move(home, "proj/a.txt", "proj/b.txt"); err != nil {
		t.Fatal(err)
	}
	if err := Move(home, "proj", "proj/inner"); err == nil {
		t.Fatal("moved folder into itself")
	}
	var buf bytes.Buffer
	if err := Download(home, "proj", &buf); err != nil {
		t.Fatal(err)
	}
	gz, _ := gzip.NewReader(&buf)
	tr := tar.NewReader(gz)
	var names []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		names = append(names, h.Name)
	}
	if strings.Join(names, ",") != "proj/,proj/b.txt,proj/bin,proj/up.bin" {
		t.Fatalf("tar %v", names)
	}
	if err := Delete(home, ""); err == nil {
		t.Fatal("deleted home")
	}
	if err := Delete(home, "etc-link"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat("/etc/passwd"); err != nil {
		t.Fatal("link target touched")
	}
	if err := Delete(home, "proj"); err != nil {
		t.Fatal(err)
	}
	if l, _ := List(home, ""); len(l) != 0 {
		t.Fatalf("left %+v", l)
	}
	if _, err := List(home, "missing"); err == nil || !strings.HasPrefix(err.Error(), "missing:") {
		t.Fatalf("error text %v", err)
	}
}
