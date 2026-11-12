package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
)

// fakeDocker answers the few Docker API calls a move makes, with volumes kept in folders.
type fakeDocker struct {
	mu         sync.Mutex
	dir        string
	containers []docker.Container
	volumes    map[string]docker.Volume
}

func (f *fakeDocker) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	p := r.URL.Path
	switch {
	case p == "/containers/json":
		json.NewEncoder(w).Encode(f.containers)
	case p == "/volumes" && r.Method == "GET":
		var vs []docker.Volume
		for _, v := range f.volumes {
			vs = append(vs, v)
		}
		json.NewEncoder(w).Encode(map[string]any{"Volumes": vs})
	case p == "/volumes/create":
		var body struct {
			Name   string
			Labels map[string]string
		}
		json.NewDecoder(r.Body).Decode(&body)
		mp := filepath.Join(f.dir, "volumes", body.Name)
		os.MkdirAll(mp, 0o755)
		v := docker.Volume{Name: body.Name, Driver: "local", Mountpoint: mp, Labels: body.Labels}
		f.volumes[body.Name] = v
		json.NewEncoder(w).Encode(v)
	case strings.HasPrefix(p, "/volumes/"):
		name := strings.TrimPrefix(p, "/volumes/")
		v, ok := f.volumes[name]
		if !ok {
			w.WriteHeader(404)
			w.Write([]byte(`{"message":"no such volume"}`))
			return
		}
		if r.Method == "DELETE" {
			os.RemoveAll(v.Mountpoint)
			delete(f.volumes, name)
			w.WriteHeader(204)
			return
		}
		json.NewEncoder(w).Encode(v)
	default:
		w.WriteHeader(404)
		w.Write([]byte(`{"message":"not faked"}`))
	}
}

func startFake(t *testing.T, f *fakeDocker) *docker.Client {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "d.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: f}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return docker.NewAt(sock)
}

func TestMoveRoundTrip(t *testing.T) {
	u, _ := user.Current()
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)

	// The old node: ~/docker/web with a compose file and data, and a named volume.
	srcHome := t.TempDir()
	src := docker.Owner{Name: u.Username, Home: srcHome, UID: uid, GID: gid}
	write := func(p, s string, mode os.FileMode) {
		os.MkdirAll(filepath.Dir(p), 0o755)
		if err := os.WriteFile(p, []byte(s), mode); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(srcHome, "docker/web/compose.yml"), "services: {}\n", 0o644)
	write(filepath.Join(srcHome, "docker/web/data/site/index.html"), "hello", 0o600)
	os.Symlink("site/index.html", filepath.Join(srcHome, "docker/web/data/link"))
	fsrc := &fakeDocker{dir: t.TempDir(), volumes: map[string]docker.Volume{}}
	vmp := filepath.Join(fsrc.dir, "volumes", "web_db")
	write(filepath.Join(vmp, "pg/PG_VERSION"), "16", 0o600)
	fsrc.volumes["web_db"] = docker.Volume{Name: "web_db", Driver: "local", Mountpoint: vmp,
		Labels: map[string]string{"com.docker.compose.project": "web", "com.docker.compose.volume": "db"}}
	fsrc.containers = []docker.Container{{ID: "c1", Names: []string{"/web-app-1"}, State: "exited",
		Labels: map[string]string{"com.docker.compose.project": "web"},
		Mounts: []docker.Mount{{Type: "bind", Source: "/srv/media", Destination: "/media"}, {Type: "volume", Name: "web_db"}},
		Ports:  []docker.Port{{PublicPort: 8080}}}}
	msrc := &Manager{dk: startFake(t, fsrc)}

	plan, err := msrc.PlanMove(src, "web")
	if err != nil {
		t.Fatal(err)
	}
	if len(plan.Volumes) != 1 || plan.Volumes[0].Name != "web_db" || len(plan.Warnings) != 1 || plan.Ports[0] != 8080 {
		t.Fatalf("plan %+v", plan)
	}

	// The new node already uses port 8080.
	dstHome := t.TempDir()
	dst := docker.Owner{Name: u.Username, Home: dstHome, UID: uid, GID: gid}
	fdst := &fakeDocker{dir: t.TempDir(), volumes: map[string]docker.Volume{}}
	fdst.containers = []docker.Container{{ID: "x", Names: []string{"/other"}, State: "running", Ports: []docker.Port{{PublicPort: 8080}}}}
	mdst := &Manager{dk: startFake(t, fdst)}
	if c := mdst.CheckMoveIn(dst, plan); len(c.Problems) != 1 || !strings.Contains(c.Problems[0], "8080") {
		t.Fatalf("check %+v", c)
	}
	fdst.containers = nil

	var buf bytes.Buffer
	if err := msrc.MoveOut(context.Background(), &buf, src, "web", false); err != nil {
		t.Fatal(err)
	}
	man, err := mdst.MoveIn(dst, bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if man.Stack != "web" || man.Folder != "web" {
		t.Fatalf("manifest %+v", man)
	}
	if b, _ := os.ReadFile(filepath.Join(dstHome, "docker/web/data/site/index.html")); string(b) != "hello" {
		t.Fatal("folder not copied")
	}
	if st, _ := os.Stat(filepath.Join(dstHome, "docker/web/data/site/index.html")); st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode())
	}
	if l, _ := os.Readlink(filepath.Join(dstHome, "docker/web/data/link")); l != "site/index.html" {
		t.Fatal("link")
	}
	v, ok := fdst.volumes["web_db"]
	if !ok || v.Labels["com.docker.compose.volume"] != "db" {
		t.Fatalf("volume %+v", fdst.volumes)
	}
	if b, _ := os.ReadFile(filepath.Join(v.Mountpoint, "pg/PG_VERSION")); string(b) != "16" {
		t.Fatal("volume not filled")
	}

	// A second copy is refused, and a broken stream leaves nothing behind.
	if _, err := mdst.MoveIn(dst, bytes.NewReader(buf.Bytes())); err == nil {
		t.Fatal("moved in over an existing stack")
	}
	mdst.AbortMoveIn(dst, "web", "web", []string{"web_db"})
	if _, err := os.Stat(filepath.Join(dstHome, "docker/web")); err == nil || len(fdst.volumes) != 0 {
		t.Fatal("abort left things behind")
	}
	if _, err := mdst.MoveIn(dst, bytes.NewReader(buf.Bytes()[:buf.Len()/2])); err == nil {
		t.Fatal("half a stream accepted")
	}
	if _, err := os.Stat(filepath.Join(dstHome, "docker/web")); err == nil || len(fdst.volumes) != 0 {
		t.Fatal("a broken move left things behind")
	}

	// On the old node the folder is marked as moved.
	if err := msrc.FinishMoveOut(src, "web", "nas", false, nil); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(srcHome, "docker/web", docker.MovedFile)); !strings.HasPrefix(string(b), "nas\n") {
		t.Fatalf("marker %q", b)
	}
}
