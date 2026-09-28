package docker

import (
	"errors"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

const (
	labelProject = "com.docker.compose.project"
	labelFiles   = "com.docker.compose.project.config_files"
	labelDir     = "com.docker.compose.project.working_dir"
)

type Project struct {
	Name       string   `json:"name"`
	Dir        string   `json:"dir"`
	Files      []string `json:"files"`
	FilesExist bool     `json:"filesExist"`
	Running    int      `json:"running"`
	Total      int      `json:"total"`
	Containers []string `json:"containers"`
}

// Projects groups containers by the labels docker compose puts on them.
func Projects(cs []Container) []Project {
	byName := map[string]*Project{}
	for _, c := range cs {
		name := c.Labels[labelProject]
		if name == "" {
			continue
		}
		p := byName[name]
		if p == nil {
			p = &Project{Name: name, Dir: c.Labels[labelDir], FilesExist: true}
			for _, f := range strings.Split(c.Labels[labelFiles], ",") {
				if f = strings.TrimSpace(f); f != "" {
					p.Files = append(p.Files, f)
					if _, err := os.Stat(f); err != nil {
						p.FilesExist = false
					}
				}
			}
			if len(p.Files) == 0 {
				p.FilesExist = false
			}
			byName[name] = p
		}
		p.Total++
		if c.State == "running" {
			p.Running++
		}
		p.Containers = append(p.Containers, c.Name())
	}
	out := make([]Project, 0, len(byName))
	for _, p := range byName {
		sort.Strings(p.Containers)
		out = append(out, *p)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Name < out[b].Name })
	return out
}

func composeArgs(p Project, action ...string) []string {
	args := []string{"compose", "-p", p.Name}
	if p.Dir != "" {
		args = append(args, "--project-directory", p.Dir)
	}
	for _, f := range p.Files {
		args = append(args, "-f", f)
	}
	return append(args, action...)
}

// ComposeAction runs up/down/pull/restart for a project found on this host.
func (c *Client) ComposeAction(name, action string) (string, error) {
	cs, err := c.Containers()
	if err != nil {
		return "", err
	}
	i := slices.IndexFunc(Projects(cs), func(p Project) bool { return p.Name == name })
	if i < 0 {
		return "", errors.New("no such compose project")
	}
	p := Projects(cs)[i]
	var verb []string
	switch action {
	case "up":
		verb = []string{"up", "-d"}
	case "down":
		verb = []string{"down"}
	case "pull":
		verb = []string{"pull"}
	case "restart":
		verb = []string{"restart"}
	default:
		return "", errors.New("unknown compose action")
	}
	// down and restart work from the project name alone; up and pull need the files.
	if !p.FilesExist && (action == "up" || action == "pull") {
		return "", errors.New("the compose files for this project aren't on this host (Portainer or another tool manages it)")
	}
	if !p.FilesExist {
		p.Files, p.Dir = nil, ""
	}
	return run.Cmd(15*time.Minute, "docker", composeArgs(p, verb...)...)
}

// ImageUpdate is one image in use by containers on this host.
type ImageUpdate struct {
	Image      string   `json:"image"`
	Containers []string `json:"containers"`
	Projects   []string `json:"projects"`
	State      string   `json:"state"` // update, uptodate, local, unreachable
	Error      string   `json:"error,omitempty"`
}

type UpdateCheck struct {
	CheckedAt int64         `json:"checkedAt"`
	Checking  bool          `json:"checking"`
	Images    []ImageUpdate `json:"images"`
	Error     string        `json:"error,omitempty"`
}

// Updater compares each image's local digest with the registry's, the same way
// docker-compose-update does (docker buildx imagetools inspect), without pulling.
type Updater struct {
	c     *Client
	mu    sync.Mutex
	check UpdateCheck
}

func NewUpdater(c *Client) *Updater { return &Updater{c: c} }

func (u *Updater) Status() UpdateCheck {
	u.mu.Lock()
	defer u.mu.Unlock()
	return u.check
}

func remoteDigest(image string) (string, error) {
	out, err := run.Cmd(60*time.Second, "docker", "buildx", "imagetools", "inspect", image, "--format", "{{.Manifest.Digest}}")
	if err != nil {
		return "", err
	}
	if !strings.HasPrefix(out, "sha256:") {
		return "", errors.New("unexpected registry answer")
	}
	return out, nil
}

func (u *Updater) Check() {
	u.mu.Lock()
	if u.check.Checking {
		u.mu.Unlock()
		return
	}
	u.check.Checking = true
	u.mu.Unlock()

	res := UpdateCheck{CheckedAt: time.Now().Unix()}
	cs, err := u.c.Containers()
	if err != nil {
		res.Error = err.Error()
	}
	byImage := map[string]*ImageUpdate{}
	var order []string
	for _, c := range cs {
		if c.Image == "" || strings.HasPrefix(c.Image, "sha256:") {
			continue
		}
		iu := byImage[c.Image]
		if iu == nil {
			iu = &ImageUpdate{Image: c.Image}
			byImage[c.Image] = iu
			order = append(order, c.Image)
		}
		iu.Containers = append(iu.Containers, c.Name())
		if p := c.Labels[labelProject]; p != "" && !slices.Contains(iu.Projects, p) {
			iu.Projects = append(iu.Projects, p)
		}
	}
	sort.Strings(order)

	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	for _, img := range order {
		iu := byImage[img]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			local, _ := u.c.ImageDigests(iu.Image)
			if len(local) == 0 {
				iu.State = "local" // built here, nothing to compare with
				return
			}
			remote, err := remoteDigest(iu.Image)
			if err != nil {
				iu.State, iu.Error = "unreachable", err.Error()
				return
			}
			iu.State = "update"
			for _, d := range local {
				if strings.HasSuffix(d, "@"+remote) {
					iu.State = "uptodate"
				}
			}
		}()
	}
	wg.Wait()
	for _, img := range order {
		res.Images = append(res.Images, *byImage[img])
	}
	u.mu.Lock()
	u.check = res
	u.mu.Unlock()
}

// Apply pulls the image and recreates the compose projects that use it. Containers
// started with plain `docker run` keep the old image until they're recreated by hand.
func (u *Updater) Apply(image string) (string, error) {
	var log []string
	out, err := run.Cmd(15*time.Minute, "docker", "pull", image)
	log = append(log, out)
	if err != nil {
		return strings.Join(log, "\n"), err
	}
	cs, err := u.c.Containers()
	if err != nil {
		return strings.Join(log, "\n"), err
	}
	standalone := 0
	for _, p := range Projects(cs) {
		uses := false
		for _, c := range cs {
			if c.Labels[labelProject] == p.Name && c.Image == image {
				uses = true
			}
		}
		if !uses {
			continue
		}
		if !p.FilesExist {
			log = append(log, "Project "+p.Name+": compose files not on this host, redeploy it where it's managed.")
			continue
		}
		out, err := run.Cmd(15*time.Minute, "docker", composeArgs(p, "up", "-d")...)
		log = append(log, out)
		if err != nil {
			return strings.Join(log, "\n"), err
		}
	}
	for _, c := range cs {
		if c.Image == image && c.Labels[labelProject] == "" {
			standalone++
		}
	}
	if standalone > 0 {
		log = append(log, "Image pulled. Containers started with docker run keep the old image until you recreate them.")
	}
	u.mu.Lock()
	for i := range u.check.Images {
		if u.check.Images[i].Image == image && standalone == 0 {
			u.check.Images[i].State = "uptodate"
		}
	}
	u.mu.Unlock()
	return strings.Join(log, "\n"), nil
}
