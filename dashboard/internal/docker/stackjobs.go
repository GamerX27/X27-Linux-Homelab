package docker

import (
	"errors"
	"fmt"
	"log"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
)

// Stack jobs run a stack action (start, stop, pull & recreate, …) in the background, so
// the UI can show its progress and output while `docker compose` pulls images, instead of
// waiting on one long request. One job runs per stack at a time.

var ErrStackBusy = errors.New("something is already running on this stack; wait for it to finish")

type StackJob struct {
	ID         int64  `json:"id"`
	Stack      string `json:"stack"`
	Action     string `json:"action"`
	Phase      string `json:"phase"` // running, checking, done, failed
	LastLine   string `json:"lastLine,omitempty"`
	Error      string `json:"error,omitempty"`
	Warning    string `json:"warning,omitempty"`   // done, but a container isn't well
	Container  string `json:"container,omitempty"` // the container the warning is about
	StartedAt  int64  `json:"startedAt"`
	FinishedAt int64  `json:"finishedAt,omitempty"`
}

func (j *StackJob) Running() bool { return j.FinishedAt == 0 }

type stackJob struct {
	job StackJob
	log []string
}

type StackJobs struct {
	c      *Client
	mu     sync.Mutex
	jobs   map[string]*stackJob
	settle time.Duration // how long containers are watched after starting
	// Done is called when a job ends (for example to refresh image update badges).
	Done func(j StackJob)
}

const (
	stackJobKeep = 30 * time.Minute // finished jobs stay listed this long
	stackLogMax  = 5000             // lines
)

var jobSeq atomic.Int64

func NewStackJobs(c *Client) *StackJobs {
	return &StackJobs{c: c, jobs: map[string]*stackJob{}, settle: 12 * time.Second}
}

// List is every running job and the jobs that finished recently, without their logs.
func (s *StackJobs) List() []StackJob {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := []StackJob{}
	for name, j := range s.jobs {
		if !j.job.Running() && time.Since(time.Unix(j.job.FinishedAt, 0)) > stackJobKeep {
			delete(s.jobs, name)
			continue
		}
		out = append(out, j.job)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Stack < out[b].Stack })
	return out
}

// Job is a stack's running or last job and its output.
func (s *StackJobs) Job(name string) (*StackJob, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.jobs[name]
	if j == nil {
		return nil, ""
	}
	cp := j.job
	return &cp, strings.Join(j.log, "\n")
}

func (s *StackJobs) update(name string, f func(j *stackJob)) {
	s.mu.Lock()
	f(s.jobs[name])
	s.mu.Unlock()
}

func (s *StackJobs) logLine(name, line string) {
	s.update(name, func(j *stackJob) {
		j.log = append(j.log, line)
		if len(j.log) > stackLogMax {
			j.log = j.log[len(j.log)-stackLogMax:]
		}
		j.job.LastLine = line
	})
}

func (s *StackJobs) phase(name, p string) { s.update(name, func(j *stackJob) { j.job.Phase = p }) }

// start runs fn as name's job. fn returns a warning for a job that worked but left
// something to look at, or an error that fails it.
func (s *StackJobs) start(name, action string, fn func() (string, error)) (StackJob, error) {
	s.mu.Lock()
	if j := s.jobs[name]; j != nil && j.job.Running() {
		s.mu.Unlock()
		return StackJob{}, ErrStackBusy
	}
	j := &stackJob{job: StackJob{ID: time.Now().UnixMilli()*1000 + jobSeq.Add(1)%1000, Stack: name, Action: action,
		Phase: "running", StartedAt: time.Now().Unix()}}
	s.jobs[name] = j
	first := j.job
	s.mu.Unlock()
	go func() {
		warning, err := fn()
		s.mu.Lock()
		j.job.FinishedAt = time.Now().Unix()
		if err != nil {
			j.job.Phase, j.job.Error = "failed", errSummary(err)
			j.log = append(j.log, "Failed: "+j.job.Error)
		} else {
			j.job.Phase, j.job.Warning = "done", warning
			if warning != "" {
				j.log = append(j.log, "Warning: "+warning)
			}
		}
		done := j.job
		s.mu.Unlock()
		if err != nil {
			log.Printf("stack %s %s failed: %s", action, name, done.Error)
		} else {
			log.Printf("stack %s %s: done%s", action, name, map[bool]string{true: " (" + warning + ")"}[warning != ""])
		}
		if s.Done != nil {
			s.Done(done)
		}
	}()
	return first, nil
}

// errSummary is the line of a compose error worth showing: the last one that names an
// error, or else the last line.
func errSummary(err error) string {
	lines := strings.Split(strings.TrimSpace(err.Error()), "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		l := strings.TrimSpace(lines[i])
		if strings.Contains(strings.ToLower(l), "error") {
			return l
		}
	}
	return strings.TrimSpace(lines[len(lines)-1])
}

// Start begins a stack action in the background. The stack is resolved first, so a
// stack that doesn't exist (or can't be started here) is an error right away.
func (s *StackJobs) Start(o Owner, name, action string, deleteFolder bool) (StackJob, error) {
	steps, err := stackSteps(action)
	if err != nil {
		return StackJob{}, err
	}
	p, err := s.c.stackProject(o, name, actionNeedsFiles(action))
	if err != nil {
		return StackJob{}, err
	}
	return s.start(name, action, func() (string, error) {
		for _, st := range steps {
			s.logLine(name, "$ docker compose "+strings.Join(st, " "))
			// Plain progress: one line per step instead of redrawn bars.
			args := append([]string{"compose", "--ansi", "never", "--progress", "plain"}, composeArgs(p, st...)[1:]...)
			if _, err := run.Stream(15*time.Minute, func(l string) { s.logLine(name, l) }, "docker", args...); err != nil {
				return "", err
			}
		}
		if action == "remove" && deleteFolder {
			msg, err := DeleteStackFolder(o, name)
			if msg != "" {
				s.logLine(name, msg)
			}
			if err != nil {
				return "", err
			}
		}
		if action == "start" || action == "recreate" || action == "restart" || action == "resume" {
			s.phase(name, "checking")
			s.logLine(name, "Checking that the containers stay up…")
			return s.watch(name), nil
		}
		return "", nil
	})
}

// ctState is what the settle check looks at in a container.
type ctState struct {
	ID, Name     string
	Status       string // running, restarting, exited, created, …
	ExitCode     int
	RestartCount int
}

func (c *Client) inspectState(id string) (ctState, error) {
	var info struct {
		Name         string `json:"Name"`
		RestartCount int    `json:"RestartCount"`
		State        struct {
			Status   string `json:"Status"`
			ExitCode int    `json:"ExitCode"`
		} `json:"State"`
	}
	if err := c.do("GET", "/containers/"+url.PathEscape(id)+"/json", nil, &info); err != nil {
		return ctState{}, err
	}
	return ctState{ID: id, Name: strings.TrimPrefix(info.Name, "/"), Status: info.State.Status,
		ExitCode: info.State.ExitCode, RestartCount: info.RestartCount}, nil
}

// trouble names what is wrong with a container compared with how it was when the check
// began, or "" when it looks fine. A container that exited with 0 did its job.
func trouble(first, now ctState) string {
	switch {
	case now.Status == "restarting" || now.RestartCount > first.RestartCount:
		if now.ExitCode != 0 {
			return fmt.Sprintf("%s keeps restarting (exit code %d)", now.Name, now.ExitCode)
		}
		return now.Name + " keeps restarting"
	case (now.Status == "exited" || now.Status == "dead") && now.ExitCode != 0:
		return fmt.Sprintf("%s stopped with exit code %d", now.Name, now.ExitCode)
	}
	return ""
}

// watch looks at the stack's containers for a few seconds after they started, and returns
// a warning (with the container's last output in the log) if one crashes or crash-loops.
func (s *StackJobs) watch(name string) string {
	first := map[string]ctState{}
	deadline := time.Now().Add(s.settle)
	for {
		cs, err := s.c.Containers()
		if err != nil {
			s.logLine(name, "Couldn't check the containers: "+err.Error())
			return ""
		}
		for _, c := range cs {
			if c.Labels[labelProject] != name {
				continue
			}
			st, err := s.c.inspectState(c.ID)
			if err != nil {
				continue
			}
			f, seen := first[c.ID]
			if !seen {
				first[c.ID], f = st, st
			}
			if msg := trouble(f, st); msg != "" {
				s.update(name, func(j *stackJob) { j.job.Container = st.Name })
				if out, err := s.c.Logs(c.ID, 30); err == nil && strings.TrimSpace(out) != "" {
					s.logLine(name, "Last output of "+st.Name+":")
					for _, l := range strings.Split(strings.TrimRight(stripTimestamps(out), "\n"), "\n") {
						s.logLine(name, "  "+l)
					}
				}
				return msg
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		time.Sleep(time.Second)
	}
	s.logLine(name, "All containers are up.")
	return ""
}

var tsRe = regexp.MustCompile(`(?m)^\d{4}-\d\d-\d\dT[0-9:.]+Z `)

func stripTimestamps(s string) string { return tsRe.ReplaceAllString(s, "") }
