package backup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
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

func TestWriteConfig(t *testing.T) {
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
	if err := WriteConfig(context.Background(), &buf, o, stacks, []string{"web"}, true, nil); err != nil {
		t.Fatal(err)
	}
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
	for _, want := range []string{"README.txt", "web/", "web/compose.yml", "web/.env", "web/data/", "web/data/site.db",
		"web/data/deep/notes.txt", "web/data/out", "Cloud/", "Cloud/compose.yaml", "Cloud/Caddyfile", "plain/", "plain/compose.yml"} {
		if got[want] == nil {
			t.Errorf("missing %s", want)
		}
	}
	for name := range got {
		if strings.HasPrefix(name, "Cloud/config") || strings.HasPrefix(name, "web/data/out/") {
			t.Errorf("unexpected %s", name)
		}
	}
	if h := got["web/data/out"]; h != nil && (h.Typeflag != tar.TypeSymlink || h.Linkname != outside) {
		t.Errorf("symlink stored as %c → %q", h.Typeflag, h.Linkname)
	}
	if h := got["web/.env"]; h != nil && (h.Mode&0o777 != 0o640 || h.Uid != o.UID || h.Uname != "") {
		t.Errorf(".env header: mode %o uid %d uname %q", h.Mode, h.Uid, h.Uname)
	}
	if content["web/.env"] != "SECRET=1\n" {
		t.Errorf(".env content: %q", content["web/.env"])
	}
	if !strings.Contains(content["README.txt"], "with their data (the whole folder): web.") {
		t.Errorf("README: %s", content["README.txt"])
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := WriteConfig(ctx, io.Discard, o, stacks, nil, false, nil); err == nil {
		t.Error("a cancelled download should fail")
	}
}
