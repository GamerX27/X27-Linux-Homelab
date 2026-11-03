package storage

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func TestEscapePath(t *testing.T) {
	for in, want := range map[string]string{
		"/var/mnt/nas-media": `var-mnt-nas\x2dmedia`,
		"/var/mnt/media":     "var-mnt-media",
		"/var/mnt/a b/":      `var-mnt-a\x20b`,
		"/":                  "-",
		"/.hidden":           `\x2ehidden`,
	} {
		if got := escapePath(in); got != want {
			t.Errorf("escapePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestClean(t *testing.T) {
	good := []Mount{
		{Name: "nas-media", Type: "nfs", Server: "192.168.1.5", Path: "/export/media/"},
		{Name: "media", Type: "NFS", Server: "nas.lan", Path: "export/media", Version: "4.2"},
		{Name: "root", Type: "nfs", Server: "nas", Path: "/"},
		{Name: "v6", Type: "nfs", Server: "[fd00::5]", Path: "/srv"},
		{Name: "films", Type: "smb", Server: "nas", Path: "/media/films", Username: "me", Domain: "WORKGROUP", FileMode: "644"},
		{Name: "public", Type: "smb", Server: "nas", Path: "public", Guest: true, Username: "ignored"},
		{Name: "admin", Type: "smb", Server: "pc", Path: "C$", Username: "admin@corp"},
	}
	for _, m := range good {
		if _, err := clean(m); err != nil {
			t.Errorf("rejected %+v: %v", m, err)
		}
	}
	c, _ := clean(good[0])
	if c.Path != "/export/media" {
		t.Errorf("trailing slash kept: %q", c.Path)
	}
	c, _ = clean(good[3])
	if c.Server != "fd00::5" || source(c) != "[fd00::5]:/srv" {
		t.Errorf("IPv6: %q %q", c.Server, source(c))
	}
	c, _ = clean(good[4])
	if c.Path != "media/films" || c.FileMode != "0644" || c.DirMode != "0775" {
		t.Errorf("smb defaults: %+v", c)
	}
	c, _ = clean(good[5])
	if c.Username != "" {
		t.Errorf("guest kept a username: %+v", c)
	}

	bad := []Mount{
		{Name: "", Type: "nfs", Server: "nas", Path: "/x"},
		{Name: "-x", Type: "nfs", Server: "nas", Path: "/x"},
		{Name: "Media", Type: "nfs", Server: "nas", Path: "/x"},
		{Name: "a/b", Type: "nfs", Server: "nas", Path: "/x"},
		{Name: "x", Type: "ftp", Server: "nas", Path: "/x"},
		{Name: "x", Type: "nfs", Server: "-nas", Path: "/x"},
		{Name: "x", Type: "nfs", Server: "nas;reboot", Path: "/x"},
		{Name: "x", Type: "nfs", Server: "nas", Path: "/a/../etc"},
		{Name: "x", Type: "nfs", Server: "nas", Path: "/a b"},
		{Name: "x", Type: "nfs", Server: "nas", Path: "/a,b"},
		{Name: "x", Type: "nfs", Server: "nas", Path: "/a\nOptions=exec"},
		{Name: "x", Type: "nfs", Server: "nas", Path: "/%h"},
		{Name: "x", Type: "nfs", Server: "nas", Path: "/x", Version: "5"},
		{Name: "x", Type: "smb", Server: "nas", Path: "media", Username: ""},
		{Name: "x", Type: "smb", Server: "nas", Path: "media", Username: "me\npassword=x"},
		{Name: "x", Type: "smb", Server: "nas", Path: "me dia", Username: "me"},
		{Name: "x", Type: "smb", Server: "nas", Path: "media", Username: "me", Domain: "a,b"},
		{Name: "x", Type: "smb", Server: "nas", Path: "media", Username: "me", FileMode: "999"},
		{Name: "x", Type: "smb", Server: "nas", Path: "media", Username: "me", Owner: "Root!"},
		{Name: "x", Type: "smb", Server: "fd00::5", Path: "media", Username: "me"},
		{Name: "x", Type: "smb", Server: "nas", Path: "media", Username: "me", Version: "1.0"},
	}
	for _, m := range bad {
		if _, err := clean(m); err == nil {
			t.Errorf("accepted %+v", m)
		}
	}
}

func TestUnitRoundTrip(t *testing.T) {
	for _, in := range []Mount{
		{Name: "nas-media", Type: "nfs", Server: "192.168.1.5", Path: "/export/media", Version: "4.2", ReadOnly: true},
		{Name: "v6", Type: "nfs", Server: "fd00::5", Path: "/"},
		{Name: "films", Type: "smb", Server: "nas.lan", Path: "media/films", Version: "3.1.1", Username: "me", Owner: "sindre", FileMode: "0664", DirMode: "0775"},
		{Name: "public", Type: "smb", Server: "nas", Path: "public", Guest: true, FileMode: "0644", DirMode: "0755", Automount: true},
	} {
		m, err := clean(in)
		if err != nil {
			t.Fatal(err)
		}
		text := mountUnit(m, fsOptions(m, "1000", "1000", credPath(m.Name)))
		got, ok := parseUnit(text)
		if !ok {
			t.Fatalf("not parsed:\n%s", text)
		}
		got.Username, got.MountPoint, got.Automount = m.Username, "", m.Automount
		if got != m {
			t.Errorf("round trip:\n got %+v\nwant %+v\n%s", got, m, text)
		}
		if m.Automount == strings.Contains(text, "[Install]") {
			t.Errorf("automount %v but [Install] is %v:\n%s", m.Automount, !m.Automount, text)
		}
	}
	if _, ok := parseUnit("[Mount]\nWhat=nas:/x\nType=nfs\n"); ok {
		t.Error("parsed a unit without the marker")
	}
}

func TestFsOptions(t *testing.T) {
	m, _ := clean(Mount{Name: "f", Type: "smb", Server: "nas", Path: "m", Username: "me", ReadOnly: true})
	want := "ro,credentials=/c,uid=1000,gid=100,file_mode=0664,dir_mode=0775,iocharset=utf8"
	if got := fsOptions(m, "1000", "100", "/c"); got != want {
		t.Errorf("got %s", got)
	}
	g, _ := clean(Mount{Name: "g", Type: "smb", Server: "nas", Path: "m", Guest: true, Version: "3.0"})
	if got := fsOptions(g, "0", "0", ""); !strings.HasPrefix(got, "rw,vers=3.0,guest,uid=0") || strings.Contains(got, "credentials") {
		t.Errorf("guest: %s", got)
	}
	n, _ := clean(Mount{Name: "n", Type: "nfs", Server: "nas", Path: "/x"})
	if got := fsOptions(n, "0", "0", ""); got != "rw" {
		t.Errorf("nfs: %s", got)
	}
}

func TestCredText(t *testing.T) {
	s, err := credText("me", "p=ss word", "WG")
	if err != nil || s != "username=me\npassword=p=ss word\ndomain=WG\n" {
		t.Errorf("%q %v", s, err)
	}
	for _, c := range [][3]string{{"", "x", ""}, {"me", "", ""}, {"me", "a\nb", ""}, {"me", "x", "a\rb"}} {
		if _, err := credText(c[0], c[1], c[2]); err == nil {
			t.Errorf("accepted %q", c)
		}
	}
	dir := t.TempDir()
	credDir = dir
	if err := writeCred("x", s); err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(credPath("x"))
	if st.Mode().Perm() != 0o600 {
		t.Errorf("mode %v", st.Mode())
	}
	if u, p, d := readCred(credPath("x")); u != "me" || p != "p=ss word" || d != "WG" {
		t.Errorf("read back %q %q %q", u, p, d)
	}
}

func TestDockerWaits(t *testing.T) {
	dockerDropIn = filepath.Join(t.TempDir(), "docker.service.d", "dashboard-mounts.conf")
	if err := setDockerWaits("/var/mnt/b", true); err != nil {
		t.Fatal(err)
	}
	setDockerWaits("/var/mnt/a", true)
	setDockerWaits("/var/mnt/a", true)
	if got := dockerWaits(); !slices.Equal(got, []string{"/var/mnt/a", "/var/mnt/b"}) {
		t.Errorf("got %v", got)
	}
	setDockerWaits("/var/mnt/a", false)
	setDockerWaits("/var/mnt/b", false)
	if _, err := os.Stat(dockerDropIn); !os.IsNotExist(err) {
		t.Error("empty drop-in left behind")
	}
	// Someone else's file at that path is left alone.
	os.WriteFile(dockerDropIn, []byte("[Unit]\nAfter=x.mount\n"), 0o644)
	setDockerWaits("/var/mnt/a", true)
	if b, _ := os.ReadFile(dockerDropIn); string(b) != "[Unit]\nAfter=x.mount\n" {
		t.Errorf("overwrote a hand-written drop-in: %s", b)
	}
}

func TestListOnlyOurs(t *testing.T) {
	unitDir, credDir = t.TempDir(), t.TempDir()
	dockerDropIn = filepath.Join(t.TempDir(), "none.conf")
	m, _ := clean(Mount{Name: "nas-media", Type: "nfs", Server: "nas", Path: "/export/media"})
	_, unit, _ := names(m.Name)
	os.WriteFile(filepath.Join(unitDir, unit), []byte(mountUnit(m, "rw")), 0o644)
	os.WriteFile(filepath.Join(unitDir, "var-mnt-other.mount"), []byte("[Mount]\nWhat=nas:/x\nWhere=/var/mnt/other\nType=nfs\n"), 0o644)
	// Ours by its marker, but under the wrong file name: not listed.
	os.WriteFile(filepath.Join(unitDir, "srv.mount"), []byte(mountUnit(m, "rw")), 0o644)
	got := List()
	if len(got) != 1 || got[0].Name != "nas-media" || got[0].Unit != unit || got[0].MountPoint != "/var/mnt/nas-media" {
		t.Errorf("got %+v", got)
	}
}

func TestParseExports(t *testing.T) {
	got := parseExports("/export/media  192.168.1.0/24\n/export/backups *\n\n")
	if !slices.Equal(got, []string{"/export/media", "/export/backups"}) {
		t.Errorf("got %v", got)
	}
}

func TestHint(t *testing.T) {
	if h := hint("mount error(13): Permission denied"); !strings.Contains(h, "password is wrong") {
		t.Errorf("got %q", h)
	}
	if h := hint("something else"); h != "something else" {
		t.Errorf("got %q", h)
	}
}
