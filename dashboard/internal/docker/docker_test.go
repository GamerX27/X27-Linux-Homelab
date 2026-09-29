package docker

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestDemux(t *testing.T) {
	b := append([]byte{1, 0, 0, 0, 0, 0, 0, 6}, "hello\n"...)
	b = append(b, append([]byte{2, 0, 0, 0, 0, 0, 0, 4}, "err\n"...)...)
	if got := Demux(b); got != "hello\nerr\n" {
		t.Fatalf("%q", got)
	}
}

func TestProjects(t *testing.T) {
	cs := []Container{
		{ID: "1", Names: []string{"/web"}, State: "running", Labels: map[string]string{labelProject: "site", labelFiles: "/nonexistent/compose.yml"}},
		{ID: "2", Names: []string{"/db"}, State: "exited", Labels: map[string]string{labelProject: "site"}},
		{ID: "3", Names: []string{"/solo"}, State: "running"},
	}
	ps := Projects(cs)
	if len(ps) != 1 || ps[0].Name != "site" || ps[0].Running != 1 || ps[0].Total != 2 || ps[0].FilesExist {
		t.Fatalf("%+v", ps)
	}
}

func TestValidID(t *testing.T) {
	for _, ok := range []string{"abc123", "ghcr.io/foo/bar:1.2", "sha256:abcd", "nginx@sha256:ab"} {
		if !ValidID(ok) {
			t.Error(ok)
		}
	}
	for _, bad := range []string{"", "../x", "a b", "-rf", "a?b"} {
		if ValidID(bad) {
			t.Error(bad)
		}
	}
}

func TestPlanUpdates(t *testing.T) {
	file := filepath.Join(t.TempDir(), "compose.yml")
	os.WriteFile(file, nil, 0o644)
	ct := func(id, name, image, proj, svc, files string) Container {
		l := map[string]string{}
		if proj != "" {
			l[labelProject], l[labelService], l[labelFiles] = proj, svc, files
		}
		return Container{ID: id, Names: []string{"/" + name}, Image: image, Labels: l}
	}
	cs := []Container{
		ct("1", "gluetun", "qmcgaw/gluetun", "arr", "gluetun", file),
		ct("2", "qbittorrent", "qbit:latest", "arr", "qbittorrent", file),
		ct("3", "seerr", "seerr:latest", "arr", "seerr", file),
		ct("4", "sonarr", "sonarr:latest", "arr", "sonarr", file),
		ct("5", "web", "seerr:latest", "portainer", "web", "/nonexistent/compose.yml"),
		ct("6", "solo", "qbit:latest", "", "", ""),
	}
	pl := planUpdates(cs, map[string]bool{"qbit:latest": true, "seerr:latest": true})
	if len(pl.Projects) != 1 || pl.Projects[0].Project.Name != "arr" ||
		!slices.Equal(pl.Projects[0].Services, []string{"qbittorrent", "seerr"}) {
		t.Fatalf("projects: %+v", pl.Projects)
	}
	if len(pl.Elsewhere) != 1 || pl.Elsewhere[0].Project.Name != "portainer" {
		t.Fatalf("elsewhere: %+v", pl.Elsewhere)
	}
	if len(pl.Standalone) != 1 || pl.Standalone[0].ID != "6" {
		t.Fatalf("standalone: %+v", pl.Standalone)
	}
}
