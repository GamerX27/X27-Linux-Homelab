package docker

import (
	"encoding/json"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"testing"
)

func testOwner(t *testing.T) Owner {
	u, _ := user.Current()
	uid, _ := strconv.Atoi(u.Uid)
	gid, _ := strconv.Atoi(u.Gid)
	return Owner{Name: u.Username, Home: t.TempDir(), UID: uid, GID: gid}
}

func TestStackNames(t *testing.T) {
	for _, ok := range []string{"jellyfin", "arr-stack", "a_b", "x1"} {
		if !ValidStackName(ok) {
			t.Error(ok)
		}
	}
	for _, bad := range []string{"", "../etc", "Jellyfin", "-x", "a/b", ".", "a b"} {
		if ValidStackName(bad) {
			t.Error(bad)
		}
	}
	for in, want := range map[string]string{"Nginx-Proxy-Manager": "nginx-proxy-manager", "My.App": "myapp", "Arr Stack": "arrstack"} {
		if got := ProjectName(in); got != want {
			t.Errorf("%q: %q", in, got)
		}
	}
}

func TestListStacksAndDir(t *testing.T) {
	o := testOwner(t)
	root := o.Root()
	os.MkdirAll(filepath.Join(root, "Jellyfin"), 0o755)
	os.WriteFile(filepath.Join(root, "Jellyfin", "compose.yml"), []byte("services: {}\n"), 0o644)
	os.MkdirAll(filepath.Join(root, "empty"), 0o755)
	cs := []Container{
		{ID: "1", Names: []string{"/jellyfin"}, State: "running", Labels: map[string]string{labelProject: "jellyfin"},
			Ports: []Port{{PublicPort: 8096, PrivatePort: 8096, Type: "tcp"}}},
		{ID: "2", Names: []string{"/portainer"}, State: "running", Labels: map[string]string{labelProject: "portainer", labelFiles: "/data/compose/1/docker-compose.yml"}},
	}
	ss := ListStacks(o, cs)
	if len(ss) != 2 {
		t.Fatalf("%+v", ss)
	}
	j, p := ss[0], ss[1]
	if j.Name != "jellyfin" || !j.InRoot || j.Running != 1 || len(j.Ports) != 1 {
		t.Fatalf("jellyfin: %+v", j)
	}
	if p.Name != "portainer" || p.InRoot || p.FilesExist {
		t.Fatalf("portainer: %+v", p)
	}
	if d, _ := StackDir(o, "jellyfin"); d != filepath.Join(root, "Jellyfin") {
		t.Fatalf("dir %s", d)
	}
	if _, err := StackDir(o, "../../etc"); err == nil {
		t.Fatal("traversal accepted")
	}
}

func TestCloneSpec(t *testing.T) {
	var inspect, image obj
	json.Unmarshal([]byte(`{
	  "Id": "abcdef1234567890",
	  "Name": "/app",
	  "Config": {"Hostname": "abcdef123456", "Image": "nginx:latest",
	    "Env": ["PATH=/usr/bin", "NGINX_VERSION=1.0", "MY_SETTING=1"],
	    "Cmd": ["nginx", "-g", "daemon off;"], "Labels": {"maintainer": "x", "mine": "y"}},
	  "HostConfig": {"Binds": ["/srv/html:/usr/share/nginx/html:ro"], "RestartPolicy": {"Name": "unless-stopped"},
	    "PortBindings": {"80/tcp": [{"HostPort": "8080"}]}},
	  "Mounts": [
	    {"Type": "bind", "Source": "/srv/html", "Destination": "/usr/share/nginx/html", "RW": false},
	    {"Type": "volume", "Name": "0123anon", "Destination": "/var/cache/nginx", "RW": true},
	    {"Type": "volume", "Name": "logs", "Destination": "/var/log/nginx", "RW": false}],
	  "NetworkSettings": {"Networks": {"web": {"Aliases": ["abcdef123456", "app"], "IPAddress": "172.18.0.5", "EndpointID": "zz"}}}
	}`), &inspect)
	json.Unmarshal([]byte(`{"Env": ["PATH=/usr/bin", "NGINX_VERSION=1.0"], "Cmd": ["nginx", "-g", "daemon off;"], "Labels": {"maintainer": "x"}}`), &image)

	spec := CloneSpec(inspect, image)
	if _, has := spec["Hostname"]; has {
		t.Error("generated hostname kept")
	}
	if _, has := spec["Cmd"]; has {
		t.Error("image Cmd pinned")
	}
	env, _ := json.Marshal(spec["Env"])
	if string(env) != `["MY_SETTING=1"]` {
		t.Errorf("env %s", env)
	}
	labels, _ := json.Marshal(spec["Labels"])
	if string(labels) != `{"mine":"y"}` {
		t.Errorf("labels %s", labels)
	}
	binds, _ := json.Marshal(spec["HostConfig"].(obj)["Binds"])
	if string(binds) != `["/srv/html:/usr/share/nginx/html:ro","0123anon:/var/cache/nginx","logs:/var/log/nginx:ro"]` {
		t.Errorf("binds %s", binds)
	}
	eps, _ := json.Marshal(spec["NetworkingConfig"])
	if string(eps) != `{"EndpointsConfig":{"web":{"Aliases":["app"]}}}` {
		t.Errorf("networks %s", eps)
	}
	if inspect["Config"].(obj)["Hostname"] != "abcdef123456" {
		t.Error("inspect was modified")
	}
}
