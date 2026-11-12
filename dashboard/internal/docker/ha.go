package docker

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

// High-availability apps (Cluster → High-availability apps) keep a copy of their stack on
// every member of the virtual IP and run only on the member that holds it. Two files in
// the stack's folder say so: HAFile, the marker, and HAOverride, a compose override the
// dashboard writes so the user's compose.yml is never edited.
const (
	HAFile     = ".x27-ha"
	HAOverride = "compose.x27-ha.yml"
)

// HAMarker is what HAFile holds, as key=value lines so docker-compose-update can read the
// virtual IP with a shell.
type HAMarker struct {
	VIP     string `json:"vip"`
	Data    string `json:"data"` // shared or replicated
	VIPOnly bool   `json:"vipOnly"`
	Here    bool   `json:"here"` // filled in by the API: this node holds the virtual IP now
}

func (m HAMarker) Text() string {
	vo := 0
	if m.VIPOnly {
		vo = 1
	}
	return fmt.Sprintf("# High-availability app (dashboard → Cluster). It runs only on the node that holds the\n"+
		"# virtual IP; the dashboard starts and stops it there.\nvip=%s\ndata=%s\nvip_only=%d\n", m.VIP, m.Data, vo)
}

func ParseHAMarker(text string) HAMarker {
	var m HAMarker
	for _, line := range strings.Split(text, "\n") {
		k, v, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.HasPrefix(k, "#") {
			continue
		}
		switch k {
		case "vip":
			m.VIP = v
		case "data":
			m.Data = v
		case "vip_only":
			m.VIPOnly = v == "1"
		}
	}
	return m
}

// ReadHA is the marker in a stack folder, or nil.
func ReadHA(dir string) *HAMarker {
	b, err := os.ReadFile(filepath.Join(dir, HAFile))
	if err != nil {
		return nil
	}
	m := ParseHAMarker(string(b))
	return &m
}

// IsHA reports whether a stack in ~/docker is a high-availability app.
func IsHA(o Owner, name string) bool {
	dir, err := StackDir(o, name)
	return err == nil && ReadHA(dir) != nil
}

// HAFiles is what the main node sends every member to keep its copy of an HA app.
type HAFiles struct {
	Compose  string   `json:"compose"`
	Env      string   `json:"env"`
	Override string   `json:"override"`
	Marker   HAMarker `json:"marker"`
}

// WriteHAFiles writes an HA app's files into its folder: compose.yml and .env (validated, as
// SaveStack does, and only when they changed), the override and the marker.
func WriteHAFiles(o Owner, name string, f HAFiles) (string, error) {
	dir, err := StackDir(o, name)
	if err != nil {
		return "", err
	}
	out := "Up to date"
	cur, err := ReadStack(o, name)
	switch {
	case err != nil:
		if _, serr := os.Stat(dir); serr == nil && findComposeFile(dir) != "" {
			return "", err
		}
		if out, err = SaveStack(o, name, f.Compose, f.Env, !exists(dir)); err != nil {
			return out, err
		}
	case cur.Compose != f.Compose || cur.Env != f.Env:
		if out, err = SaveStack(o, name, f.Compose, f.Env, false); err != nil {
			return out, err
		}
	}
	if err := ensureDir(o, dir); err != nil {
		return "", err
	}
	if err := writeOwned(o, filepath.Join(dir, HAOverride), []byte(f.Override), 0o644); err != nil {
		return "", err
	}
	if err := writeOwned(o, filepath.Join(dir, HAFile), []byte(f.Marker.Text()), 0o644); err != nil {
		return "", err
	}
	return out, nil
}

func exists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// RemoveHAFiles turns a stack back into an ordinary one: the marker and the override go,
// compose.yml and the data stay.
func RemoveHAFiles(o Owner, name string) error {
	dir, err := StackDir(o, name)
	if err != nil {
		return err
	}
	for _, f := range []string{HAFile, HAOverride} {
		if err := os.Remove(filepath.Join(dir, f)); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	return nil
}

// ComposeFile is the part of `docker compose config --format json` the cluster checks.
type ComposeFile struct {
	Name     string                    `json:"name"`
	Services map[string]ComposeService `json:"services"`
	Volumes  map[string]ComposeVolume  `json:"volumes"`
}

type ComposeService struct {
	Ports   []ComposePort  `json:"ports"`
	Volumes []ComposeMount `json:"volumes"`
	Restart string         `json:"restart"`
}

type ComposePort struct {
	Mode      string `json:"mode,omitempty"`
	HostIP    string `json:"host_ip,omitempty"`
	Target    int    `json:"target"`
	Published string `json:"published,omitempty"`
	Protocol  string `json:"protocol,omitempty"`
	Name      string `json:"name,omitempty"`
}

type ComposeMount struct {
	Type   string `json:"type"` // bind, volume, tmpfs…
	Source string `json:"source"`
	Target string `json:"target"`
}

type ComposeVolume struct {
	Name       string            `json:"name"`
	Driver     string            `json:"driver"`
	DriverOpts map[string]string `json:"driver_opts"`
	External   bool              `json:"external"`
}

// ComposeConfig is a stack's compose file as compose resolves it (paths absolute, ports in
// long form), without the HA override.
func ComposeConfig(o Owner, name string) (ComposeFile, error) {
	dir, err := StackDir(o, name)
	if err != nil {
		return ComposeFile{}, err
	}
	f := findComposeFile(dir)
	if f == "" {
		return ComposeFile{}, errors.New("no compose file in " + dir)
	}
	out, err := run.Cmd(30*time.Second, "docker", "compose", "-p", name, "--project-directory", dir, "-f", f, "config", "--format", "json")
	if err != nil {
		return ComposeFile{}, fmt.Errorf("the compose file isn't valid: %v %s", err, out)
	}
	return ParseComposeConfig(out)
}

// ParseComposeConfig reads compose's JSON. Older compose versions print "published" as a
// number; both are accepted.
func ParseComposeConfig(text string) (ComposeFile, error) {
	var raw struct {
		Name     string `json:"name"`
		Services map[string]struct {
			Ports []struct {
				ComposePort
				Published json.RawMessage `json:"published"`
			} `json:"ports"`
			Volumes []ComposeMount `json:"volumes"`
			Restart string         `json:"restart"`
		} `json:"services"`
		Volumes map[string]ComposeVolume `json:"volumes"`
	}
	if i := strings.Index(text, "{"); i > 0 {
		text = text[i:] // warnings before the JSON
	}
	if err := json.Unmarshal([]byte(text), &raw); err != nil {
		return ComposeFile{}, fmt.Errorf("reading compose's config: %w", err)
	}
	cf := ComposeFile{Name: raw.Name, Services: map[string]ComposeService{}, Volumes: raw.Volumes}
	for name, s := range raw.Services {
		svc := ComposeService{Volumes: s.Volumes, Restart: s.Restart}
		for _, p := range s.Ports {
			cp := p.ComposePort
			if len(p.Published) > 0 && string(p.Published) != "null" {
				var str string
				if json.Unmarshal(p.Published, &str) == nil {
					cp.Published = str
				} else {
					cp.Published = strings.Trim(string(p.Published), `"`)
				}
			}
			svc.Ports = append(svc.Ports, cp)
		}
		cf.Services[name] = svc
	}
	return cf, nil
}
