package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
)

type entry struct {
	name, body, link string
	typ              byte
	mode             int64
	uid, gid         int
}

// makeExport builds an export .tar.gz from entries, with a server.json first.
func makeExport(t *testing.T, man Manifest, entries ...entry) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	js, _ := json.Marshal(man)
	tw.WriteHeader(&tar.Header{Name: "server.json", Mode: 0o600, Size: int64(len(js)), Typeflag: tar.TypeReg})
	tw.Write(js)
	mtime := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, e := range entries {
		if e.typ == 0 {
			e.typ = tar.TypeReg
		}
		if e.mode == 0 {
			e.mode = 0o644
		}
		hdr := &tar.Header{Name: e.name, Typeflag: e.typ, Mode: e.mode, Linkname: e.link, ModTime: mtime,
			Uid: e.uid, Gid: e.gid, Size: int64(len(e.body))}
		if e.typ != tar.TypeReg {
			hdr.Size = 0
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if e.typ == tar.TypeReg {
			tw.Write([]byte(e.body))
		}
	}
	tw.Close()
	gz.Close()
	return buf.Bytes()
}

func same(uid, gid int) (int, int) { return uid, gid }

func TestUnpackRefusesEscapes(t *testing.T) {
	base := t.TempDir()
	rootDir := filepath.Join(base, "docker")
	outside := filepath.Join(base, "outside")
	os.MkdirAll(rootDir, 0o755)
	os.MkdirAll(outside, 0o755)
	me, mg := os.Getuid(), os.Getgid()
	archive := makeExport(t, Manifest{Version: 1},
		entry{name: "docker/good/", typ: tar.TypeDir, mode: 0o755, uid: me, gid: mg},
		entry{name: "docker/good/compose.yml", body: "services: {}\n", uid: me, gid: mg},
		entry{name: "docker/dots/", typ: tar.TypeDir, mode: 0o755, uid: me, gid: mg},
		entry{name: "docker/dots/../../outside/dots", body: "x", uid: me, gid: mg},
		entry{name: "/docker/abs/etc", body: "x", uid: me, gid: mg},
		entry{name: "docker/link/", typ: tar.TypeDir, mode: 0o755, uid: me, gid: mg},
		entry{name: "docker/link/out", typ: tar.TypeSymlink, link: outside, uid: me, gid: mg},
		entry{name: "docker/link/out/pwned", body: "x", uid: me, gid: mg},
		entry{name: "docker/abslink/", typ: tar.TypeDir, mode: 0o755, uid: me, gid: mg},
		entry{name: "docker/abslink/up", typ: tar.TypeSymlink, link: "../..", uid: me, gid: mg},
		entry{name: "docker/abslink/up/outside/up", body: "x", uid: me, gid: mg},
	)
	root, err := os.OpenRoot(rootDir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	want := map[string]bool{"good": true, "dots": true, "abs": true, "link": true, "abslink": true}
	errs := Unpack(bytes.NewReader(archive), root, want, same)

	if errs["good"] != nil {
		t.Errorf("good: %v", errs["good"])
	}
	for _, a := range []string{"dots", "link", "abslink"} {
		if errs[a] == nil {
			t.Errorf("%s: an escaping entry should fail the app", a)
		}
	}
	if left, _ := os.ReadDir(outside); len(left) > 0 {
		t.Errorf("wrote outside the root: %v", left)
	}
	if _, err := os.Stat(filepath.Join(base, "docker", "abs")); err == nil {
		t.Error("an absolute entry was unpacked")
	}
	// The symlink itself is recreated, pointing where it did.
	if l, err := os.Readlink(filepath.Join(rootDir, "link", "out")); err != nil || l != outside {
		t.Errorf("symlink: %q %v", l, err)
	}
	if b, _ := os.ReadFile(filepath.Join(rootDir, "good", "compose.yml")); string(b) != "services: {}\n" {
		t.Errorf("good/compose.yml: %q", b)
	}
}

func TestUnpackKeepsModesAndOwners(t *testing.T) {
	rootDir := t.TempDir()
	uid, gid := os.Getuid(), os.Getgid()
	src := uid
	if uid == 0 {
		src = 1000 // as root: check that the exporting user's ids are mapped, others kept
	}
	archive := makeExport(t, Manifest{Version: 1},
		entry{name: "app/", typ: tar.TypeDir}, // outside docker/: ignored
		entry{name: "docker/app/", typ: tar.TypeDir, mode: 0o750, uid: src, gid: src},
		entry{name: "docker/app/.env", body: "SECRET=1\n", mode: 0o600, uid: src, gid: src},
		entry{name: "docker/app/run.sh", body: "#!/bin/sh\n", mode: 0o4755, uid: src, gid: src},
		entry{name: "docker/app/data/", typ: tar.TypeDir, mode: 0o1777, uid: src, gid: src},
		entry{name: "docker/app/data/db", body: "rows", mode: 0o644, uid: 999, gid: 999},
		entry{name: "docker/other/compose.yml", body: "x"}, // not ticked
	)
	root, _ := os.OpenRoot(rootDir)
	defer root.Close()
	uidMap := func(u, g int) (int, int) {
		if u == src {
			u = uid
		}
		if g == src {
			g = gid
		}
		return u, g
	}
	if uid != 0 {
		uidMap = func(int, int) (int, int) { return uid, gid } // can't chown to 999 without root
	}
	errs := Unpack(bytes.NewReader(archive), root, map[string]bool{"app": true}, uidMap)
	if errs["app"] != nil {
		t.Fatal(errs["app"])
	}
	check := func(rel string, mode os.FileMode) os.FileInfo {
		st, err := os.Lstat(filepath.Join(rootDir, rel))
		if err != nil {
			t.Fatal(err)
		}
		if st.Mode()&(os.ModePerm|os.ModeSticky|os.ModeSetuid) != mode {
			t.Errorf("%s: mode %v, want %v", rel, st.Mode(), mode)
		}
		if !st.ModTime().Equal(time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)) {
			t.Errorf("%s: mtime %v", rel, st.ModTime())
		}
		return st
	}
	check("app", 0o750)
	check("app/.env", 0o600)
	check("app/run.sh", 0o755) // setuid dropped
	check("app/data", 0o777|os.ModeSticky)
	db := check("app/data/db", 0o644)
	if uid == 0 {
		if st := ownerOf(db); st[0] != 999 {
			t.Errorf("app/data/db owner %v, want 999 kept", st)
		}
		if st := ownerOf(check("app/.env", 0o600)); st[0] != 0 {
			t.Errorf("app/.env owner %v, want the importing user (0)", st)
		}
	}
	if _, err := os.Stat(filepath.Join(rootDir, "other")); err == nil {
		t.Error("an app that wasn't ticked was unpacked")
	}
}

func TestImportSkipsExistingUnlessReplace(t *testing.T) {
	config.StateDir = t.TempDir()
	home := t.TempDir()
	o := docker.Owner{Name: "test", Home: home, UID: os.Getuid(), GID: os.Getgid()}
	os.MkdirAll(filepath.Join(o.Root(), "web", "data"), 0o755)
	os.WriteFile(filepath.Join(o.Root(), "web", "compose.yml"), []byte("old"), 0o644)
	os.WriteFile(filepath.Join(o.Root(), "web", "data", "keep"), []byte("data"), 0o644)
	os.WriteFile(filepath.Join(o.Root(), "web", ".env"), []byte("old env"), 0o600)
	me := os.Getuid()
	archive := makeExport(t, Manifest{Version: 1, Host: "old-box", Taken: 1700000000, Owner: &ManifestOwner{"test", me, os.Getgid()}},
		entry{name: "docker/web/", typ: tar.TypeDir, mode: 0o755, uid: me},
		entry{name: "docker/web/compose.yml", body: "new", uid: me},
		entry{name: "docker/web/.env", typ: tar.TypeSymlink, link: "/etc/shadow", uid: me}, // replaces the file, not what it'd point to
		entry{name: "docker/cloud/", typ: tar.TypeDir, mode: 0o755, uid: me},
		entry{name: "docker/cloud/compose.yml", body: "cloud", uid: me},
		entry{name: "docker/cloud/data/x", body: "x", uid: me},
	)
	m := &Manager{path: filepath.Join(config.StateDir, "backups.json"), cfg: Config{Keep: DefaultKeep, Freq: "off"}}

	run := func(replace bool) Job {
		pv, err := m.Upload(o, bytes.NewReader(archive))
		if err != nil {
			t.Fatal(err)
		}
		// cloud is only there the second time, after the first import.
		if pv.Host != "old-box" || len(pv.Apps) != 2 || pv.Apps[0].Name != "cloud" || !pv.Apps[0].Data || !pv.Apps[1].Exists || pv.Apps[0].Exists != replace {
			t.Fatalf("preview: %+v", pv)
		}
		if _, err := m.StartImport(o, ImportRequest{ID: "nope", Apps: []string{"web"}}); err == nil {
			t.Error("a wrong id should be refused")
		}
		if _, err := m.StartImport(o, ImportRequest{ID: pv.ID, Apps: []string{"web", "cloud"}, Replace: replace}); err != nil {
			t.Fatal(err)
		}
		m.running.Lock() // wait for the job
		m.running.Unlock()
		if m.imp != nil {
			t.Error("the upload should be deleted once applied")
		}
		return *m.job
	}

	j := run(false)
	if b, _ := os.ReadFile(filepath.Join(o.Root(), "web", "compose.yml")); string(b) != "old" {
		t.Errorf("an existing app was written over without Replace: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(o.Root(), "cloud", "data", "x")); string(b) != "x" {
		t.Errorf("cloud wasn't restored: %q", b)
	}
	if j.Phase != "done" || j.Summary != "Imported 1 app" {
		t.Errorf("job: %+v\n%s", j, m.Log())
	}

	j = run(true)
	if b, _ := os.ReadFile(filepath.Join(o.Root(), "web", "compose.yml")); string(b) != "new" {
		t.Errorf("Replace didn't write over compose.yml: %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(o.Root(), "web", "data", "keep")); string(b) != "data" {
		t.Error("Replace should keep files the export doesn't have")
	}
	if l, err := os.Readlink(filepath.Join(o.Root(), "web", ".env")); err != nil || l != "/etc/shadow" {
		t.Errorf(".env should now be the symlink: %q %v", l, err)
	}
	if j.Summary != "Imported 2 apps" || !strings.Contains(m.Log(), "web: restored") {
		t.Errorf("job: %+v\n%s", j, m.Log())
	}
}

func TestReadExportNeedsManifest(t *testing.T) {
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	tw.WriteHeader(&tar.Header{Name: "web/compose.yml", Mode: 0o644, Typeflag: tar.TypeReg})
	tw.Close()
	gz.Close()
	if _, _, err := ReadExport(&buf); err == nil || !strings.Contains(err.Error(), "server.json") {
		t.Errorf("an old config download should be refused: %v", err)
	}
	if _, _, err := ReadExport(strings.NewReader("not gzip")); err == nil {
		t.Error("garbage should be refused")
	}
	newer := makeExport(t, Manifest{Version: ManifestVersion + 1})
	if _, _, err := ReadExport(bytes.NewReader(newer)); err == nil {
		t.Error("a newer format should be refused")
	}
}

func TestParseParts(t *testing.T) {
	p, err := ParseParts([]string{"dashboard,shares", "network"})
	if err != nil || !p.Dashboard || !p.Shares || p.Updates || !p.Network || p.String() != "dashboard, shares, network" {
		t.Errorf("%+v %v", p, err)
	}
	if _, err := ParseParts([]string{"passwords"}); err == nil {
		t.Error("unknown part accepted")
	}
}

func ownerOf(st os.FileInfo) [2]int {
	s := st.Sys().(*syscall.Stat_t)
	return [2]int{int(s.Uid), int(s.Gid)}
}
