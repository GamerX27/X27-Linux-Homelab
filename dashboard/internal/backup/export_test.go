package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
)

func TestIsConfigFile(t *testing.T) {
	dir := t.TempDir()
	write := func(name string, size int) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, make([]byte, size), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	small := write("compose.yml", 100)
	edge := write("exact", maxConfigFile)
	big := write("big.db", maxConfigFile+1)
	sub := filepath.Join(dir, "data")
	os.Mkdir(sub, 0o755)
	link := filepath.Join(dir, "link.yml")
	os.Symlink(small, link)
	for _, tc := range []struct {
		p    string
		want bool
	}{{small, true}, {edge, true}, {big, false}, {sub, false}, {link, false}} {
		info, err := os.Lstat(tc.p)
		if err != nil {
			t.Fatal(err)
		}
		if got := isConfigFile(info); got != tc.want {
			t.Errorf("%s: got %v, want %v", filepath.Base(tc.p), got, tc.want)
		}
	}
}

func TestWriteExport(t *testing.T) {
	home := t.TempDir()
	o := docker.Owner{Name: "test", Home: home, UID: os.Getuid(), GID: os.Getgid()}
	root := o.Root()
	files := map[string]string{
		"web/compose.yml":         "services: {}\n",
		"web/.env":                "SECRET=1\n",
		"web/data/site.db":        "rows",
		"web/data/deep/notes.txt": "deep",
		"Cloud/compose.yaml":      "services: {}\n",
		"Cloud/Caddyfile":         ":80\n",
		"Cloud/config/app.ini":    "x=1",
		"plain/compose.yml":       "services: {}\n",
	}
	for p, c := range files {
		full := filepath.Join(root, p)
		os.MkdirAll(filepath.Dir(full), 0o755)
		if err := os.WriteFile(full, []byte(c), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	outside := filepath.Join(home, "outside")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "secret"), []byte("no"), 0o600)
	os.Symlink(outside, filepath.Join(root, "web/data/out"))

	apps := DataApps(o, nil)
	names := []string{}
	for _, a := range apps {
		names = append(names, a.Name)
	}
	if !slices.Equal(names, []string{"cloud", "web"}) {
		t.Fatalf("data apps: %v", names)
	}
	if apps[1].Size != int64(len("rows")+len("deep")) {
		t.Errorf("web data size: %d", apps[1].Size)
	}

	stacks := rootStacks(o, nil)
	var buf bytes.Buffer
	man := Manifest{Updates: &UpdatesPart{GotifyURL: "https://gotify.example", GotifyToken: "tok", Services: map[string]bool{"ssh": true}},
		Network: &NetworkPart{Hostname: "box", Timezone: "Europe/Oslo", NTP: true,
			Connections: []NetConn{{Name: "Wired connection 1", Device: "eth0", Method: "manual", Addresses: []string{"192.168.1.10/24"}}}}}
	if err := WriteExport(context.Background(), &buf, o, stacks, []string{"web"}, true, nil, man); err != nil {
		t.Fatal(err)
	}
	archive := slices.Clone(buf.Bytes())
	gz, err := gzip.NewReader(&buf)
	if err != nil {
		t.Fatal(err)
	}
	tr := tar.NewReader(gz)
	got := map[string]*tar.Header{}
	content := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		got[hdr.Name] = hdr
		b, _ := io.ReadAll(tr)
		content[hdr.Name] = string(b)
	}
	for _, want := range []string{"README.txt", "server.json", "docker/web/", "docker/web/compose.yml", "docker/web/.env", "docker/web/data/",
		"docker/web/data/site.db", "docker/web/data/deep/notes.txt", "docker/web/data/out", "docker/Cloud/", "docker/Cloud/compose.yaml",
		"docker/Cloud/Caddyfile", "docker/plain/", "docker/plain/compose.yml"} {
		if got[want] == nil {
			t.Errorf("missing %s", want)
		}
	}
	for name := range got {
		if strings.HasPrefix(name, "docker/Cloud/config") || strings.HasPrefix(name, "docker/web/data/out/") ||
			(name != "README.txt" && name != "server.json" && !strings.HasPrefix(name, "docker/")) {
			t.Errorf("unexpected %s", name)
		}
	}
	if h := got["docker/web/data/out"]; h != nil && (h.Typeflag != tar.TypeSymlink || h.Linkname != outside) {
		t.Errorf("symlink stored as %c → %q", h.Typeflag, h.Linkname)
	}
	if h := got["docker/web/.env"]; h != nil && (h.Mode&0o777 != 0o640 || h.Uid != o.UID || h.Uname != "") {
		t.Errorf(".env header: mode %o uid %d uname %q", h.Mode, h.Uid, h.Uname)
	}
	if content["docker/web/.env"] != "SECRET=1\n" {
		t.Errorf(".env content: %q", content["docker/web/.env"])
	}
	if !strings.Contains(content["README.txt"], "with their data (the whole folder): web.") ||
		!strings.Contains(content["README.txt"], "HOLDS SECRETS") || strings.Contains(content["README.txt"], "network shares") ||
		!strings.Contains(content["README.txt"], "hostname, time zone") {
		t.Errorf("README: %s", content["README.txt"])
	}
	if h := got["server.json"]; h == nil || h.Mode != 0o600 {
		t.Errorf("server.json should be private: %+v", h)
	}

	// server.json comes back as written, and ReadExport sees the apps.
	var back Manifest
	if err := json.Unmarshal([]byte(content["server.json"]), &back); err != nil {
		t.Fatal(err)
	}
	if back.Version != ManifestVersion || back.Owner == nil || back.Owner.UID != o.UID || back.Dashboard != nil || back.Shares != nil {
		t.Errorf("manifest: %+v", back)
	}
	if back.Updates == nil || back.Updates.GotifyToken != "tok" || !back.Updates.Services["ssh"] {
		t.Errorf("updates part: %+v", back.Updates)
	}
	if back.Network == nil || back.Network.Connections[0].Addresses[0] != "192.168.1.10/24" {
		t.Errorf("network part: %+v", back.Network)
	}
	wantApps := []ManifestApp{{"Cloud", false}, {"plain", false}, {"web", true}}
	slices.SortFunc(back.Apps, func(a, b ManifestApp) int { return strings.Compare(a.Name, b.Name) })
	if !slices.Equal(back.Apps, wantApps) {
		t.Errorf("manifest apps: %v", back.Apps)
	}
	if !slices.Equal(back.Present(), []string{"updates", "network"}) {
		t.Errorf("present: %v", back.Present())
	}
	_, apps2, err := ReadExport(bytes.NewReader(archive))
	if err != nil {
		t.Fatal(err)
	}
	if len(apps2) != 3 || apps2[0].Name != "Cloud" || apps2[0].Data || !apps2[2].Data || apps2[2].Size == 0 {
		t.Errorf("ReadExport apps: %+v", apps2)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WriteExport(ctx, io.Discard, o, stacks, nil, false, nil, Manifest{}); err == nil {
		t.Error("a cancelled download should fail")
	}
}
