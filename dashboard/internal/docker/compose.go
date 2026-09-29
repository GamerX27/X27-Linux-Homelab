package docker

import (
	"errors"
	"fmt"
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
	labelService = "com.docker.compose.service"
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
	c        *Client
	mu       sync.Mutex
	check    UpdateCheck
	applying sync.Mutex // one update run at a time
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

// projectUpdate is one compose project an update touches: the services whose images
// changed, and those images.
type projectUpdate struct {
	Project  Project
	Services []string
	Images   []string
}

// updatePlan groups the containers using a set of images by what updates them: compose
// projects (all their changed services in one pass), standalone containers, and projects
// whose compose files aren't on this host (Portainer), which have to be redeployed there.
type updatePlan struct {
	Projects   []projectUpdate
	Standalone []Container
	Elsewhere  []projectUpdate
}

func planUpdates(cs []Container, images map[string]bool) updatePlan {
	var pl updatePlan
	for _, p := range Projects(cs) {
		pu := projectUpdate{Project: p}
		for _, c := range cs {
			if c.Labels[labelProject] != p.Name || !images[c.Image] {
				continue
			}
			if svc := c.Labels[labelService]; svc != "" && !slices.Contains(pu.Services, svc) {
				pu.Services = append(pu.Services, svc)
			}
			if !slices.Contains(pu.Images, c.Image) {
				pu.Images = append(pu.Images, c.Image)
			}
		}
		if len(pu.Images) == 0 {
			continue
		}
		sort.Strings(pu.Services)
		if p.FilesExist {
			pl.Projects = append(pl.Projects, pu)
		} else {
			pl.Elsewhere = append(pl.Elsewhere, pu)
		}
	}
	for _, c := range cs {
		if c.Labels[labelProject] == "" && images[c.Image] {
			pl.Standalone = append(pl.Standalone, c)
		}
	}
	return pl
}

// updateProject pulls the given services (all of them when none are given) and runs one
// `up -d`, so compose recreates exactly the containers whose image changed plus the ones
// that depend on them (depends_on, network_mode: service:x), and leaves the rest alone.
func updateProject(p Project, services []string) (string, error) {
	out, err := run.Cmd(15*time.Minute, "docker", composeArgs(p, append([]string{"pull"}, services...)...)...)
	if err != nil {
		return out, err
	}
	out2, err := run.Cmd(15*time.Minute, "docker", composeArgs(p, "up", "-d")...)
	return strings.TrimSpace(out + "\n" + out2), err
}

type ApplyAllResult struct {
	Updated int    `json:"updated"`
	Failed  int    `json:"failed"`
	Output  string `json:"output"`
}

// applyPlan updates each project and standalone container in the plan, and marks the
// images whose users all updated as up to date.
func (u *Updater) applyPlan(pl updatePlan) ApplyAllResult {
	var res ApplyAllResult
	var log []string
	stale := map[string]bool{} // images some user of which wasn't updated
	for _, pu := range pl.Projects {
		log = append(log, "== "+pu.Project.Name+": "+strings.Join(pu.Services, ", "))
		out, err := updateProject(pu.Project, pu.Services)
		log = append(log, out)
		if err != nil {
			res.Failed++
			log = append(log, "failed: "+err.Error())
			for _, img := range pu.Images {
				stale[img] = true
			}
			continue
		}
		res.Updated++
	}
	for _, c := range pl.Standalone {
		log = append(log, "== "+c.Name())
		out, err := u.c.Recreate(c.ID)
		log = append(log, out)
		if err != nil {
			res.Failed++
			log = append(log, "failed: "+err.Error())
			stale[c.Image] = true
			continue
		}
		res.Updated++
	}
	for _, pu := range pl.Elsewhere {
		log = append(log, "== "+pu.Project.Name+": compose files not on this host, redeploy it where it's managed.")
		for _, img := range pu.Images {
			stale[img] = true
		}
	}
	done := map[string]bool{}
	for _, pu := range pl.Projects {
		for _, img := range pu.Images {
			done[img] = true
		}
	}
	for _, c := range pl.Standalone {
		done[c.Image] = true
	}
	u.mu.Lock()
	for i, img := range u.check.Images {
		if done[img.Image] && !stale[img.Image] {
			u.check.Images[i].State = "uptodate"
		}
	}
	u.mu.Unlock()
	res.Output = strings.TrimSpace(strings.Join(log, "\n"))
	return res
}

var ErrUpdateRunning = errors.New("an update is already running on this node")

// Apply updates what uses one image: each compose project through updateProject, and
// containers started with plain `docker run` through Recreate (same settings and volumes).
func (u *Updater) Apply(image string) (string, error) {
	if !u.applying.TryLock() {
		return "", ErrUpdateRunning
	}
	defer u.applying.Unlock()
	cs, err := u.c.Containers()
	if err != nil {
		return "", err
	}
	res := u.applyPlan(planUpdates(cs, map[string]bool{image: true}))
	if res.Failed > 0 {
		return res.Output, fmt.Errorf("%d of %d failed", res.Failed, res.Failed+res.Updated)
	}
	return res.Output, nil
}

// ApplyAll checks every image against its registry again and updates the ones that
// changed, one pass per stack. Containers whose images are up to date aren't touched.
func (u *Updater) ApplyAll() (ApplyAllResult, error) {
	if !u.applying.TryLock() {
		return ApplyAllResult{}, ErrUpdateRunning
	}
	defer u.applying.Unlock()
	for u.Status().Checking { // a check started elsewhere; wait for it, then check fresh
		time.Sleep(time.Second)
	}
	u.Check()
	st := u.Status()
	if st.Error != "" {
		return ApplyAllResult{}, errors.New(st.Error)
	}
	images := map[string]bool{}
	for _, img := range st.Images {
		if img.State == "update" {
			images[img.Image] = true
		}
	}
	cs, err := u.c.Containers()
	if err != nil {
		return ApplyAllResult{}, err
	}
	res := u.applyPlan(planUpdates(cs, images))
	if res.Updated == 0 && res.Failed == 0 && res.Output == "" {
		res.Output = "All images are up to date."
	}
	return res, nil
}

// UpdateStack updates the services of one stack whose images have updates (per the last
// check), in one pass. With none known it pulls every service and runs `up -d`.
func (u *Updater) UpdateStack(o Owner, name string) (string, error) {
	if !u.applying.TryLock() {
		return "", ErrUpdateRunning
	}
	defer u.applying.Unlock()
	p, err := u.c.stackProject(o, name, true)
	if err != nil {
		return "", err
	}
	cs, err := u.c.Containers()
	if err != nil {
		return "", err
	}
	images := map[string]bool{}
	for _, img := range u.Status().Images {
		if img.State == "update" {
			images[img.Image] = true
		}
	}
	var services []string
	for _, pu := range planUpdates(cs, images).Projects {
		if pu.Project.Name == name {
			services = pu.Services
		}
	}
	return updateProject(p, services)
}
