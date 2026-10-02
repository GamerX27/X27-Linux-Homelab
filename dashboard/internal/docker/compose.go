package docker

import (
	"errors"
	"fmt"
	"log"
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
	Job       *UpdateJob    `json:"job,omitempty"` // the running update, or the last one
}

// UpdateJob is one update run in the background. The UI polls it to show what is being
// updated right now, what is queued, and how the run ended; its output is in Log.
type UpdateJob struct {
	ID         int64    `json:"id"`
	Kind       string   `json:"kind"`             // all, stack, image
	Target     string   `json:"target,omitempty"` // the stack or image
	Phase      string   `json:"phase"`            // starting, checking, pulling, recreating, done, failed
	Current    string   `json:"current,omitempty"`
	Pending    []string `json:"pending,omitempty"` // stacks and standalone containers still to do
	Updated    int      `json:"updated"`
	Failed     int      `json:"failed"`
	Error      string   `json:"error,omitempty"`
	StartedAt  int64    `json:"startedAt"`
	FinishedAt int64    `json:"finishedAt,omitempty"`
}

func (j *UpdateJob) Running() bool { return j.FinishedAt == 0 }

// Updater compares each image's local digest with the registry's, the same way
// docker-compose-update does (docker buildx imagetools inspect), without pulling.
type Updater struct {
	c        *Client
	mu       sync.Mutex
	check    UpdateCheck
	job      *UpdateJob
	log      []string
	applying sync.Mutex // one update run at a time
}

func NewUpdater(c *Client) *Updater { return &Updater{c: c} }

func (u *Updater) Status() UpdateCheck {
	u.mu.Lock()
	defer u.mu.Unlock()
	st := u.check
	st.Images = slices.Clone(st.Images)
	if u.job != nil {
		j := *u.job
		j.Pending = slices.Clone(j.Pending)
		st.Job = &j
	}
	return st
}

// Log is the output of the running or last update.
func (u *Updater) Log() string {
	u.mu.Lock()
	defer u.mu.Unlock()
	return strings.Join(u.log, "\n")
}

func (u *Updater) logf(format string, a ...any) {
	s := strings.TrimSpace(fmt.Sprintf(format, a...))
	if s == "" {
		return
	}
	u.mu.Lock()
	u.log = append(u.log, s)
	u.mu.Unlock()
}

func (u *Updater) setJob(f func(j *UpdateJob)) {
	u.mu.Lock()
	f(u.job)
	u.mu.Unlock()
}

func (u *Updater) phase(p string) { u.setJob(func(j *UpdateJob) { j.Phase = p }) }

// begin marks what is updated next: it leaves the queue and becomes the current one.
func (u *Updater) begin(name, phase string) {
	u.setJob(func(j *UpdateJob) {
		j.Current, j.Phase = name, phase
		j.Pending = slices.DeleteFunc(j.Pending, func(p string) bool { return p == name })
	})
}

// start runs fn as the update job in the background and returns the job as it starts.
// fn counts what it updated and failed on the job; an error from it fails the whole run.
func (u *Updater) start(kind, target string, fn func() error) (UpdateJob, error) {
	if !u.applying.TryLock() {
		return UpdateJob{}, ErrUpdateRunning
	}
	u.mu.Lock()
	u.job = &UpdateJob{ID: time.Now().UnixMilli(), Kind: kind, Target: target, Phase: "starting", StartedAt: time.Now().Unix()}
	u.log = nil
	j := *u.job
	u.mu.Unlock()
	go func() {
		defer u.applying.Unlock()
		err := fn()
		u.mu.Lock()
		defer u.mu.Unlock()
		j := u.job
		j.FinishedAt, j.Current, j.Pending = time.Now().Unix(), "", nil
		switch {
		case err != nil:
			j.Phase, j.Error = "failed", err.Error()
			u.log = append(u.log, "failed: "+err.Error())
		case j.Failed > 0:
			j.Phase, j.Error = "failed", fmt.Sprintf("%d of %d failed", j.Failed, j.Failed+j.Updated)
		default:
			j.Phase = "done"
		}
		log.Printf("container update (%s %s): %s, %d updated, %d failed", kind, target, j.Phase, j.Updated, j.Failed)
	}()
	return j, nil
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
func (u *Updater) updateProject(p Project, services []string) error {
	u.phase("pulling")
	out, err := run.Cmd(15*time.Minute, "docker", composeArgs(p, append([]string{"pull"}, services...)...)...)
	u.logOutput(out, err)
	if err != nil {
		return err
	}
	u.phase("recreating")
	out, err = run.Cmd(15*time.Minute, "docker", composeArgs(p, "up", "-d")...)
	u.logOutput(out, err)
	return err
}

// logOutput logs a command's output, and its error when that says more than the output
// (run.Cmd's error usually is the output).
func (u *Updater) logOutput(out string, err error) {
	u.logf("%s", out)
	if err != nil && err.Error() != out {
		u.logf("failed: %v", err)
	} else if err != nil {
		u.logf("failed")
	}
}

// applyPlan updates each project and standalone container in the plan, counting them on
// the job, and marks the images whose users all updated as up to date.
func (u *Updater) applyPlan(pl updatePlan) {
	var queue []string
	for _, pu := range pl.Projects {
		queue = append(queue, pu.Project.Name)
	}
	for _, c := range pl.Standalone {
		queue = append(queue, c.Name())
	}
	u.setJob(func(j *UpdateJob) { j.Pending = queue })

	stale := map[string]bool{} // images some user of which wasn't updated
	count := func(err error) {
		u.setJob(func(j *UpdateJob) {
			if err != nil {
				j.Failed++
			} else {
				j.Updated++
			}
		})
	}
	for _, pu := range pl.Projects {
		u.begin(pu.Project.Name, "pulling")
		u.logf("== %s: %s", pu.Project.Name, strings.Join(pu.Services, ", "))
		err := u.updateProject(pu.Project, pu.Services)
		count(err)
		if err != nil {
			for _, img := range pu.Images {
				stale[img] = true
			}
		}
	}
	for _, c := range pl.Standalone {
		u.begin(c.Name(), "recreating")
		u.logf("== %s", c.Name())
		out, err := u.c.Recreate(c.ID)
		u.logOutput(out, err)
		count(err)
		if err != nil {
			stale[c.Image] = true
		}
	}
	for _, pu := range pl.Elsewhere {
		u.logf("== %s: compose files not on this host, redeploy it where it's managed.", pu.Project.Name)
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
}

var ErrUpdateRunning = errors.New("an update is already running on this node")

// knownUpdates is the images the last check found updates for.
func (u *Updater) knownUpdates() map[string]bool {
	images := map[string]bool{}
	for _, img := range u.Status().Images {
		if img.State == "update" {
			images[img.Image] = true
		}
	}
	return images
}

// StartApply updates what uses one image in the background: each compose project through
// updateProject, and containers started with plain `docker run` through Recreate (same
// settings and volumes).
func (u *Updater) StartApply(image string) (UpdateJob, error) {
	return u.start("image", image, func() error {
		cs, err := u.c.Containers()
		if err != nil {
			return err
		}
		u.applyPlan(planUpdates(cs, map[string]bool{image: true}))
		return nil
	})
}

// StartApplyAll checks every image against its registry again and updates the ones that
// changed, one pass per stack. Containers whose images are up to date aren't touched.
func (u *Updater) StartApplyAll() (UpdateJob, error) {
	return u.start("all", "", func() error {
		u.phase("checking")
		for u.Status().Checking { // a check started elsewhere; wait for it, then check fresh
			time.Sleep(time.Second)
		}
		u.Check()
		if st := u.Status(); st.Error != "" {
			return errors.New(st.Error)
		}
		images := u.knownUpdates()
		if len(images) == 0 {
			u.logf("All images are up to date.")
			return nil
		}
		cs, err := u.c.Containers()
		if err != nil {
			return err
		}
		u.applyPlan(planUpdates(cs, images))
		return nil
	})
}

// StartUpdateStack updates the services of one stack whose images have updates (per the
// last check), in one pass. With none known it pulls every service and runs `up -d`.
func (u *Updater) StartUpdateStack(o Owner, name string) (UpdateJob, error) {
	p, err := u.c.stackProject(o, name, true)
	if err != nil {
		return UpdateJob{}, err
	}
	return u.start("stack", name, func() error {
		cs, err := u.c.Containers()
		if err != nil {
			return err
		}
		for _, pu := range planUpdates(cs, u.knownUpdates()).Projects {
			if pu.Project.Name == name {
				pu.Project = p
				u.applyPlan(updatePlan{Projects: []projectUpdate{pu}})
				return nil
			}
		}
		u.begin(name, "pulling")
		u.logf("== %s: all services", name)
		err = u.updateProject(p, nil)
		u.setJob(func(j *UpdateJob) {
			if err != nil {
				j.Failed++
			} else {
				j.Updated++
			}
		})
		return nil
	})
}
