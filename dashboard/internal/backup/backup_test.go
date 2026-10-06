package backup

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
)

func TestNext(t *testing.T) {
	loc := time.UTC
	// Tuesday 2026-10-06 10:00
	now := time.Date(2026, 10, 6, 10, 0, 0, 0, loc)
	for _, tc := range []struct {
		c    Config
		want time.Time
	}{
		{Config{Freq: "daily", Time: "03:30"}, time.Date(2026, 10, 7, 3, 30, 0, 0, loc)},
		{Config{Freq: "daily", Time: "11:00"}, time.Date(2026, 10, 6, 11, 0, 0, 0, loc)},
		{Config{Freq: "weekly", Weekday: "sun", Time: "04:00"}, time.Date(2026, 10, 11, 4, 0, 0, 0, loc)},
		{Config{Freq: "weekly", Weekday: "tue", Time: "10:00"}, time.Date(2026, 10, 13, 10, 0, 0, 0, loc)},
		{Config{Freq: "monthly", Day: "31", Time: "02:00"}, time.Date(2026, 10, 31, 2, 0, 0, 0, loc)},
		{Config{Freq: "monthly", Day: "1", Time: "02:00"}, time.Date(2026, 11, 1, 2, 0, 0, 0, loc)},
		{Config{Freq: "off", Time: "02:00"}, time.Time{}},
	} {
		if got := Next(tc.c, now); !got.Equal(tc.want) {
			t.Errorf("%+v: got %v, want %v", tc.c, got, tc.want)
		}
	}
	// November has no 31st: the next one is in December.
	nov := time.Date(2026, 11, 1, 0, 0, 0, 0, loc)
	if got := Next(Config{Freq: "monthly", Day: "31", Time: "02:00"}, nov); !got.Equal(time.Date(2026, 12, 31, 2, 0, 0, 0, loc)) {
		t.Errorf("monthly 31 after Nov 1: %v", got)
	}
}

func TestValidDest(t *testing.T) {
	o := docker.Owner{Name: "u", Home: "/home/u"}
	for _, ok := range []string{"/home/u/backups/docker", "/mnt/nas/backups", "/home/u/dockerbackups"} {
		if err := ValidDest(o, ok); err != nil {
			t.Errorf("%s: %v", ok, err)
		}
	}
	for _, bad := range []string{"backups", "/", "/home/u/docker", "/home/u/docker/backups", "/home/u/docker/../docker/x"} {
		if ValidDest(o, bad) == nil {
			t.Errorf("%s accepted", bad)
		}
	}
}

func TestPruneAndArchives(t *testing.T) {
	dest := t.TempDir()
	dir := filepath.Join(dest, "web")
	os.MkdirAll(dir, 0o700)
	names := []string{"web-20261001-030000.tar.gz", "web-20261002-030000.tar.gz", "web-20261003-030000.tar.gz",
		"other-20261001-030000.tar.gz", "notes.txt", "web-20261004-030000.tar.gz.partial"}
	for _, n := range names {
		os.WriteFile(filepath.Join(dir, n), []byte("x"), 0o600)
	}
	gone := Prune(dir, "web", 2)
	if !slices.Equal(gone, []string{"web-20261001-030000.tar.gz"}) {
		t.Fatalf("pruned %v", gone)
	}
	for _, n := range names[1:] {
		if _, err := os.Stat(filepath.Join(dir, n)); err != nil {
			t.Errorf("%s was deleted", n)
		}
	}
	as := Archives(dest)
	if len(as) != 2 || as[0].File != "web-20261003-030000.tar.gz" || as[0].App != "web" || as[0].Size != 1 {
		t.Fatalf("%+v", as)
	}
}
