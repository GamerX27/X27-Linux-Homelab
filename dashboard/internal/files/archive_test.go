package files

import (
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

func read(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestCompressFormatsRoundTrip(t *testing.T) {
	for format, ext := range CompressFormats {
		t.Run(format, func(t *testing.T) {
			if format == "tar.zst" {
				if _, err := exec.LookPath("zstd"); err != nil {
					t.Skip("no zstd")
				}
			}
			home := t.TempDir()
			write(t, filepath.Join(home, "p/site/index.html"), "hi", 0o644)
			write(t, filepath.Join(home, "p/site/run.sh"), "#!/bin/sh", 0o755)
			os.Symlink("index.html", filepath.Join(home, "p/site/link"))
			write(t, filepath.Join(home, "p/site/.dashboard-zip-123"), "temp", 0o600)

			var last, total int64
			p, err := CompressWith(home, CompressOpts{Paths: []string{"p/site"}, Name: "backup", Format: format, To: "",
				Progress: func(d, tot int64) { last, total = d, tot }})
			if err != nil || p != "p/backup"+ext {
				t.Fatal(p, err)
			}
			if total != 11 || last != 11 {
				t.Fatalf("progress %d of %d", last, total)
			}
			l, err := ListArchive(home, p)
			if err != nil {
				t.Fatal(err)
			}
			var names []string
			for _, e := range l.Entries {
				names = append(names, e.Name)
			}
			sort.Strings(names)
			if strings.Join(names, ",") != "site,site/index.html,site/link,site/run.sh" || l.Top != "site" {
				t.Fatalf("listed %v top %q", names, l.Top)
			}
			r, err := ExtractWith(home, ExtractOpts{Path: p, Into: IntoTo, To: "out"})
			if err == nil {
				t.Fatal("extracted into a folder that doesn't exist")
			}
			os.Mkdir(filepath.Join(home, "out"), 0o755)
			if r, err = ExtractWith(home, ExtractOpts{Path: p, Into: IntoTo, To: "out"}); err != nil || r.Path != "out" {
				t.Fatal(r, err)
			}
			if read(t, filepath.Join(home, "out/site/index.html")) != "hi" {
				t.Fatal("content")
			}
			if st, _ := os.Stat(filepath.Join(home, "out/site/run.sh")); st.Mode().Perm() != 0o755 {
				t.Fatalf("mode %v", st.Mode())
			}
			if l, _ := os.Readlink(filepath.Join(home, "out/site/link")); l != "index.html" {
				t.Fatal("link")
			}
			ents, _ := os.ReadDir(filepath.Join(home, "out"))
			if len(ents) != 1 {
				t.Fatalf("temporary folder left behind: %v", ents)
			}
		})
	}
}

func TestCompressNames(t *testing.T) {
	home := t.TempDir()
	write(t, filepath.Join(home, "a.txt"), "a", 0o644)
	os.Mkdir(filepath.Join(home, "dst"), 0o755)
	if p, err := CompressWith(home, CompressOpts{Paths: []string{"a.txt"}, Name: "x.tar.gz", Format: "tar.gz", To: "dst"}); err != nil || p != "dst/x.tar.gz" {
		t.Fatal(p, err)
	}
	if p, err := CompressWith(home, CompressOpts{Paths: []string{"a.txt"}, Name: "x", Format: "tar.gz", To: "dst"}); err != nil || p != "dst/x (2).tar.gz" {
		t.Fatal(p, err)
	}
	if _, err := CompressWith(home, CompressOpts{Paths: []string{"a.txt"}, Name: "../x"}); err == nil {
		t.Fatal("name with a slash accepted")
	}
	if _, err := CompressWith(home, CompressOpts{Paths: []string{"a.txt"}, Format: "rar"}); err == nil {
		t.Fatal("unknown format accepted")
	}
}

func TestExtractConflicts(t *testing.T) {
	setup := func(t *testing.T) string {
		home := t.TempDir()
		mkzip(t, filepath.Join(home, "a.zip"), map[string]string{"conf/app.yml": "new", "conf/extra.yml": "x", "notes.txt": "new"}, nil)
		write(t, filepath.Join(home, "conf/app.yml"), "old", 0o644)
		write(t, filepath.Join(home, "notes.txt"), "old", 0o644)
		return home
	}
	t.Run("keep", func(t *testing.T) {
		home := setup(t)
		r, err := ExtractWith(home, ExtractOpts{Path: "a.zip", Into: IntoHere})
		if err != nil || r.Path != "" || r.Renamed != 2 {
			t.Fatal(r, err)
		}
		if read(t, filepath.Join(home, "conf/app.yml")) != "old" || read(t, filepath.Join(home, "conf/app (2).yml")) != "new" ||
			read(t, filepath.Join(home, "notes (2).txt")) != "new" || read(t, filepath.Join(home, "conf/extra.yml")) != "x" {
			t.Fatal("keep both")
		}
	})
	t.Run("skip", func(t *testing.T) {
		home := setup(t)
		r, err := ExtractWith(home, ExtractOpts{Path: "a.zip", Into: IntoHere, Conflict: Skip})
		if err != nil || r.Skipped != 2 {
			t.Fatal(r, err)
		}
		if read(t, filepath.Join(home, "notes.txt")) != "old" || read(t, filepath.Join(home, "conf/extra.yml")) != "x" {
			t.Fatal("skip")
		}
	})
	t.Run("overwrite", func(t *testing.T) {
		home := setup(t)
		os.Mkdir(filepath.Join(home, "elsewhere"), 0o755)
		os.Symlink(filepath.Join(home, "elsewhere"), filepath.Join(home, "linked"))
		mkzip(t, filepath.Join(home, "b.zip"), map[string]string{"linked/x": "x"}, nil)
		r, err := ExtractWith(home, ExtractOpts{Path: "a.zip", Into: IntoHere, Conflict: Overwrite})
		if err != nil || r.Replaced != 2 {
			t.Fatal(r, err)
		}
		if read(t, filepath.Join(home, "notes.txt")) != "new" || read(t, filepath.Join(home, "conf/app.yml")) != "new" {
			t.Fatal("overwrite")
		}
		// A link in the destination is never followed: the folder is kept next to it.
		if r, err := ExtractWith(home, ExtractOpts{Path: "b.zip", Into: IntoHere, Conflict: Overwrite}); err != nil || r.Renamed != 1 {
			t.Fatal(r, err)
		}
		if _, err := os.Stat(filepath.Join(home, "elsewhere/x")); err == nil {
			t.Fatal("wrote through a link")
		}
	})
	t.Run("folder name", func(t *testing.T) {
		home := setup(t)
		if r, err := ExtractWith(home, ExtractOpts{Path: "a.zip", Name: "unpacked"}); err != nil || r.Path != "unpacked" {
			t.Fatal(r, err)
		}
		if _, err := ExtractWith(home, ExtractOpts{Path: "a.zip", Name: "a/b"}); err == nil {
			t.Fatal("folder name with a slash accepted")
		}
	})
}

func TestTopFolder(t *testing.T) {
	l := ArchiveList{Entries: []ArchiveEntry{{Name: "a.txt"}}}
	if topFolder(l) != "" {
		t.Fatal("one file is not a folder")
	}
	l.Entries = []ArchiveEntry{{Name: "app", Dir: true}, {Name: "app/x"}}
	if topFolder(l) != "app" {
		t.Fatal("app")
	}
	l.Entries = append(l.Entries, ArchiveEntry{Name: "b"})
	if topFolder(l) != "" {
		t.Fatal("two at the top")
	}
}
