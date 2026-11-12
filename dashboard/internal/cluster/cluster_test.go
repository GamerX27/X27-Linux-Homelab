package cluster

import (
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/storage"
)

func tempState(t *testing.T) {
	t.Helper()
	old := config.StateDir
	config.StateDir = t.TempDir()
	mu.Lock()
	cached = nil
	mu.Unlock()
	t.Cleanup(func() {
		config.StateDir = old
		mu.Lock()
		cached = nil
		mu.Unlock()
	})
}

func TestStateUpdateAndCopies(t *testing.T) {
	tempState(t)
	if s := Load(); s.Rev != 0 || len(s.Shares) != 0 {
		t.Fatalf("%+v", s)
	}
	s, err := Update(func(s *State) error {
		s.VIP = VIPConfig{Enabled: true, Address: "10.0.0.5", Prefix: 24, Password: "vrrp", Members: Priorities([]Member{{NodeID: LocalID}, {NodeID: "a"}, {NodeID: "b"}})}
		s.Shares = append(s.Shares, Share{Mount: storage.Mount{Name: "media", Type: "smb"}, Password: "hunter2"})
		return nil
	})
	if err != nil || s.Rev != 1 || s.Secret == "" {
		t.Fatalf("%+v %v", s, err)
	}
	if p := s.VIP.Members; p[0].Priority != 150 || p[1].Priority != 140 || p[2].Priority != 130 {
		t.Fatalf("priorities %+v", p)
	}
	if _, err := Update(func(s *State) error { s.VIP.Address = "x"; return errors.New("no") }); err == nil || Load().VIP.Address != "10.0.0.5" {
		t.Fatal("a failed update was kept")
	}
	m := Load().ForMembers()
	if m.VIP.Password != "" || m.Shares[0].Password != "" || !m.Shares[0].HasPassword || m.Secret == "" {
		t.Fatalf("member copy %+v", m)
	}
	if p := Load().Public(); p.Secret != "" {
		t.Fatal("the browser saw the secret")
	}
	// A member stores the copy it gets.
	b, _ := json.Marshal(m)
	if err := Receive(b); err != nil || Load().Shares[0].Password != "" {
		t.Fatal("receive")
	}
}

func TestSelf(t *testing.T) {
	for _, c := range []struct{ self, id, want string }{
		{LocalID, "a", "a"}, {"", LocalID, LocalID}, {"a", "a", LocalID}, {"a", LocalID, MainPeer}, {"a", "b", "b"},
	} {
		if got := Self(c.self, c.id); got != c.want {
			t.Errorf("Self(%q, %q) = %q, want %q", c.self, c.id, got, c.want)
		}
	}
}

func compose(t *testing.T, js string) docker.ComposeFile {
	t.Helper()
	cf, err := docker.ParseComposeConfig(js)
	if err != nil {
		t.Fatal(err)
	}
	return cf
}

func TestCheckData(t *testing.T) {
	members := []string{LocalID, "a"}
	shares := []Share{
		{Mount: storage.Mount{Name: "media"}},                                    // every node
		{Mount: storage.Mount{Name: "apps"}, Nodes: []string{LocalID, "a", "b"}}, // every member
		{Mount: storage.Mount{Name: "only-here"}, Nodes: []string{LocalID}},
	}
	ok := compose(t, `{"name":"jf","services":{"jellyfin":{"volumes":[
		{"type":"bind","source":"/var/mnt/apps/jellyfin/config","target":"/config"},
		{"type":"bind","source":"/var/mnt/media","target":"/media"},
		{"type":"bind","source":"/etc/localtime","target":"/etc/localtime"},
		{"type":"volume","source":"cache","target":"/cache"}]}},
		"volumes":{"cache":{"name":"jf_cache","driver_opts":{"type":"nfs","o":"addr=10.0.0.9","device":":/cache"}}}}`)
	c := CheckData(ok, shares, members, "/var/mnt", "/home/me/docker/jf")
	if len(c.Problems) != 0 || strings.Join(c.Shares, ",") != "apps,media" {
		t.Fatalf("%+v", c)
	}

	bad := compose(t, `{"name":"jf","services":{"web":{"volumes":[
		{"type":"bind","source":"/home/me/docker/jf/config","target":"/config"},
		{"type":"bind","source":"/var/mnt/only-here/x","target":"/x"},
		{"type":"bind","source":"/var/mnt/unknown","target":"/u"},
		{"type":"bind","source":"/srv/data","target":"/data"},
		{"type":"volume","source":"db","target":"/db"}]}},"volumes":{"db":{"name":"jf_db"}}}`)
	c = CheckData(bad, shares, members, "/var/mnt", "/home/me/docker/jf")
	if len(c.Problems) != 5 {
		t.Fatalf("problems %q", c.Problems)
	}
	for i, want := range []string{"stack's folder", "isn't mounted on every member", "isn't a cluster share", "/srv/data is on this node's disk", "volume db"} {
		if !strings.Contains(c.Problems[i], want) {
			t.Errorf("problem %d: %q lacks %q", i, c.Problems[i], want)
		}
	}
	// A replicated app copies its folder and volumes; the rest it warns about.
	if len(c.Warnings) != 3 || !strings.Contains(c.Warnings[2], "/srv/data") {
		t.Fatalf("warnings %q", c.Warnings)
	}
}

func TestOverride(t *testing.T) {
	cf := compose(t, `{"name":"jf","services":{
		"jellyfin":{"ports":[{"mode":"ingress","target":8096,"published":"8096","protocol":"tcp"},
			{"target":7359,"published":7359,"protocol":"udp","host_ip":"0.0.0.0"},
			{"target":9000,"published":"9000","host_ip":"127.0.0.1","protocol":"tcp"}]},
		"helper":{}}}`)
	out := Override(cf, "192.168.1.50", true)
	for _, want := range []string{
		"services:\n  \"helper\":\n    restart: on-failure\n  \"jellyfin\":\n    restart: on-failure\n    ports: !override\n",
		"      - target: 8096\n        published: \"8096\"\n        host_ip: \"192.168.1.50\"\n        protocol: tcp\n        mode: ingress\n",
		"      - target: 7359\n        published: \"7359\"\n        host_ip: \"192.168.1.50\"\n        protocol: udp\n",
		"      - target: 9000\n        published: \"9000\"\n        host_ip: \"127.0.0.1\"\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in\n%s", want, out)
		}
	}
	if out := Override(cf, "192.168.1.50", false); strings.Contains(out, "ports") || !strings.Contains(out, "restart: on-failure") {
		t.Errorf("without VIP-only:\n%s", out)
	}
}

func TestLock(t *testing.T) {
	dir := t.TempDir()
	p := lockPath(dir, "apps", "jf")
	if p != filepath.Join(dir, "apps/.x27-ha/jf.lock") {
		t.Fatal(p)
	}
	now := time.Unix(1_000_000, 0)
	if err := writeLock(p, "node-a", now); err != nil {
		t.Fatal(err)
	}
	l, err := readLock(p)
	if err != nil || l.Node != "node-a" || l.At != now.Unix() {
		t.Fatalf("%+v %v", l, err)
	}
	if who := lockBlocker(l, "node-b", now.Add(10*time.Second)); who != "node-a" {
		t.Fatal("a fresh lock didn't block")
	}
	if lockBlocker(l, "node-a", now.Add(10*time.Second)) != "" {
		t.Fatal("its own lock blocked a node")
	}
	if lockBlocker(l, "node-b", now.Add(lockStale)) != "" {
		t.Fatal("a stale lock blocked")
	}
	removeLock(p, "node-b") // not ours: stays
	if _, err := readLock(p); err != nil {
		t.Fatal("removed someone else's lock")
	}
	removeLock(p, "node-a")
	if _, err := readLock(p); err == nil {
		t.Fatal("lock not removed")
	}
}

// fakeNodes keeps each node's mounts in memory, like storage's fake mode does for one.
type fakeNodes struct {
	mu     sync.Mutex
	list   []NodeInfo
	mounts map[string]map[string]storage.Mount
	down   map[string]bool
	calls  []string
}

func (f *fakeNodes) List() []NodeInfo { return f.list }

func (f *fakeNodes) JSON(user, id, method, path string, body, out any, timeout time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.down[id] {
		return errors.New("unreachable")
	}
	ms := f.mounts[id]
	if method == "GET" {
		var list []storage.Mount
		for _, m := range ms {
			list = append(list, m)
		}
		b, _ := json.Marshal(map[string]any{"mounts": list})
		return json.Unmarshal(b, out)
	}
	f.calls = append(f.calls, id+" "+path)
	switch {
	case strings.HasSuffix(path, "/remove"):
		delete(ms, strings.TrimSuffix(strings.TrimPrefix(path, "/storage/mounts/"), "/remove"))
	default:
		mb := body.(mountBody)
		if mb.Server == "fail.example" {
			return fmt.Errorf("mount error(112): Host is down")
		}
		mb.Mount.State = "mounted"
		mb.Mount.HasPassword = mb.Password != ""
		ms[mb.Name] = mb.Mount
	}
	return nil
}

func TestSharesReconcile(t *testing.T) {
	tempState(t)
	f := &fakeNodes{list: []NodeInfo{{LocalID, "tower"}, {"a", "nas"}, {"b", "mini"}},
		mounts: map[string]map[string]storage.Mount{LocalID: {}, "a": {}, "b": {"own": {Name: "own", Type: "nfs", State: "mounted"}}},
		down:   map[string]bool{}}
	r := NewShares(f)
	media := Share{Mount: storage.Mount{Name: "media", Type: "nfs", Server: "10.0.0.9", Path: "/export/media", DockerWaits: true}}
	Update(func(s *State) error { s.Shares = []Share{media}; return nil })
	r.Once()
	for _, id := range []string{LocalID, "a", "b"} {
		if m, ok := f.mounts[id]["media"]; !ok || !m.Cluster || !m.DockerWaits {
			t.Fatalf("media on %s: %+v", id, f.mounts[id])
		}
	}
	st, _ := r.Status()
	if len(st["media"]) != 3 || st["media"][1].State != "mounted" || st["media"][1].Name != "nas" {
		t.Fatalf("status %+v", st)
	}

	// Nothing changed: no calls. Then only two nodes, and read-only.
	f.calls = nil
	r.Once()
	if len(f.calls) != 0 {
		t.Fatalf("needless calls %v", f.calls)
	}
	Update(func(s *State) error {
		s.Shares[0].Nodes = []string{LocalID, "a"}
		s.Shares[0].ReadOnly = true
		return nil
	})
	r.Once()
	if _, ok := f.mounts["b"]["media"]; ok {
		t.Fatal("not removed from the node no longer targeted")
	}
	if _, ok := f.mounts["b"]["own"]; !ok {
		t.Fatal("removed a node's own share")
	}
	if !f.mounts["a"]["media"].ReadOnly {
		t.Fatal("change not applied")
	}

	// A node that's off, a share that fails, and a name clash with a node's own share.
	f.down["a"] = true
	Update(func(s *State) error {
		s.Shares = append(s.Shares, Share{Mount: storage.Mount{Name: "bad", Type: "nfs", Server: "fail.example", Path: "/x"}},
			Share{Mount: storage.Mount{Name: "own", Type: "nfs", Server: "10.0.0.9", Path: "/own"}, Nodes: []string{"b"}})
		return nil
	})
	r.Once()
	st, _ = r.Status()
	byNode := func(share, node string) NodeShare {
		for _, ns := range st[share] {
			if ns.Node == node {
				return ns
			}
		}
		return NodeShare{}
	}
	if ns := byNode("media", "a"); ns.State != "offline" {
		t.Errorf("offline node: %+v", ns)
	}
	if ns := byNode("bad", LocalID); ns.State != "failed" || !strings.Contains(ns.Error, "Host is down") {
		t.Errorf("failing share: %+v", ns)
	}
	if ns := byNode("own", "b"); ns.State != "failed" || !strings.Contains(ns.Error, "of its own") {
		t.Errorf("clash: %+v", ns)
	}

	// Removing the share from the cluster removes it from every node.
	f.down["a"] = false
	Update(func(s *State) error { s.Shares = nil; return nil })
	r.Once()
	for _, id := range []string{LocalID, "a"} {
		if _, ok := f.mounts[id]["media"]; ok {
			t.Errorf("media still on %s", id)
		}
	}
}
