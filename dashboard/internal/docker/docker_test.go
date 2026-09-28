package docker

import "testing"

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
