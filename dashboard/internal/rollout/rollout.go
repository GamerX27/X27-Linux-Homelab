// Package rollout runs OS updates across nodes from the main node: every node downloads
// and stages the new image at the same time, then they reboot one at a time, each only
// after the one before came back on the new image. The main node goes last. A node that
// fails stops the rollout before the next reboot; the rest stay staged.
//
// The job is saved after every step (rollout.json), so it outlives a closed browser and the
// main node's own reboot: on start, the dashboard picks the job up where it was.
package rollout

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/osupdate"
)

// Nodes is how the rollout reaches a node's OS update API (local or through its token).
type Nodes interface {
	Status(id string) (osupdate.Status, error)
	Stage(id string) error
	Apply(id string) error
}

// Job states.
const (
	Running = "running"
	Stopped = "stopped" // by request or because a node failed; staged nodes can still continue
	Done    = "done"
	Failed  = "failed" // finished, but some node failed
)

// Node steps.
const (
	StepWaiting   = "waiting"
	StepStaging   = "staging"
	StepStaged    = "staged"
	StepUpToDate  = "uptodate"
	StepRebooting = "rebooting"
	StepVerifying = "verifying"
	StepDone      = "done"
	StepFailed    = "failed"
)

type Target struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Local bool   `json:"local"`
}

type Node struct {
	Target
	Step           string `json:"step"`
	Msg            string `json:"msg"` // what it's doing now: rpm-ostree progress, "offline", …
	Error          string `json:"error,omitempty"`
	FailedStep     string `json:"failedStep,omitempty"` // the step it was on when it failed
	From           string `json:"from"`
	To             string `json:"to"`
	ToName         string `json:"toName"`
	TargetChecksum string `json:"-"`
	PrevJob        string `json:"-"` // the node's update job before we started one
	JobID          string `json:"-"`
	BootBefore     string `json:"-"`
	WentDown       bool   `json:"wentDown"`
	StepAt         int64  `json:"stepAt"` // when the current step started
	FinishedAt     int64  `json:"finishedAt,omitempty"`
}

type Job struct {
	ID            string  `json:"id"`
	User          string  `json:"user"`
	State         string  `json:"state"`
	StartedAt     int64   `json:"startedAt"`
	FinishedAt    int64   `json:"finishedAt,omitempty"`
	StopRequested bool    `json:"stopRequested"`
	Error         string  `json:"error,omitempty"`
	Nodes         []*Node `json:"nodes"`
}

// persisted keeps the fields the API leaves out.
type persisted struct {
	Job
	Nodes []persistedNode `json:"nodes"`
}

type persistedNode struct {
	*Node
	TargetChecksum string `json:"targetChecksum"`
	PrevJob        string `json:"prevJob"`
	JobID          string `json:"jobId"`
	BootBefore     string `json:"bootBefore"`
}

type Manager struct {
	path  string
	nodes Nodes

	// Timings; tests shorten them.
	Poll          time.Duration
	StartTimeout  time.Duration // for the node's update run to show up
	StageTimeout  time.Duration
	RebootTimeout time.Duration

	mu      sync.Mutex
	job     *Job
	running bool
}

// New loads the saved job and carries on with it if it was still running.
func New(path string, nodes Nodes) *Manager {
	m := &Manager{path: path, nodes: nodes, Poll: 3 * time.Second, StartTimeout: 2 * time.Minute,
		StageTimeout: 45 * time.Minute, RebootTimeout: 15 * time.Minute}
	if b, err := os.ReadFile(path); err == nil {
		var p persisted
		if err := json.Unmarshal(b, &p); err != nil {
			log.Printf("rollout: can't read %s: %v", path, err)
		} else {
			j := p.Job
			j.Nodes = nil
			for _, pn := range p.Nodes {
				n := pn.Node
				n.TargetChecksum, n.PrevJob, n.JobID, n.BootBefore = pn.TargetChecksum, pn.PrevJob, pn.JobID, pn.BootBefore
				j.Nodes = append(j.Nodes, n)
			}
			m.job = &j
		}
	}
	return m
}

// Resume continues a job that was running when the dashboard stopped.
func (m *Manager) Resume() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != nil && m.job.State == Running && !m.running {
		log.Printf("rollout: resuming update %s", m.job.ID)
		m.running = true
		go m.run()
	}
}

func (m *Manager) saveLocked() {
	p := persisted{Job: *m.job}
	for _, n := range m.job.Nodes {
		p.Nodes = append(p.Nodes, persistedNode{n, n.TargetChecksum, n.PrevJob, n.JobID, n.BootBefore})
	}
	b, _ := json.MarshalIndent(p, "", "  ")
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		err = os.Rename(tmp, m.path)
	}
}

// update changes the job under the lock and saves it.
func (m *Manager) update(fn func(j *Job)) {
	m.mu.Lock()
	defer m.mu.Unlock()
	fn(m.job)
	m.saveLocked()
}

func (m *Manager) setStep(n *Node, step, msg string) {
	m.update(func(*Job) {
		if n.Step != step {
			n.StepAt = time.Now().Unix()
		}
		n.Step, n.Msg = step, msg
		switch step {
		case StepDone, StepFailed, StepUpToDate:
			n.FinishedAt = time.Now().Unix()
		}
	})
}

func (m *Manager) fail(n *Node, err string) {
	m.update(func(*Job) { n.Error, n.FailedStep = err, n.Step })
	m.setStep(n, StepFailed, "")
}

// Get returns a copy of the current (or last) job, nil if there's none.
func (m *Manager) Get() *Job {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job == nil {
		return nil
	}
	j := *m.job
	j.Nodes = make([]*Node, len(m.job.Nodes))
	for i, n := range m.job.Nodes {
		c := *n
		j.Nodes[i] = &c
	}
	return &j
}

// Start begins an update of targets. Only one runs at a time.
func (m *Manager) Start(user string, targets []Target) (*Job, error) {
	if len(targets) == 0 {
		return nil, errors.New("no nodes to update")
	}
	m.mu.Lock()
	if m.job != nil && (m.job.State == Running || m.running) {
		m.mu.Unlock()
		return nil, errors.New("an update is already running")
	}
	now := time.Now()
	j := &Job{ID: fmt.Sprint(now.UnixNano()), User: user, State: Running, StartedAt: now.Unix()}
	for _, t := range targets {
		j.Nodes = append(j.Nodes, &Node{Target: t, Step: StepWaiting, StepAt: now.Unix()})
	}
	m.job = j
	m.running = true
	m.saveLocked()
	m.mu.Unlock()
	go m.run()
	return m.Get(), nil
}

// Stop lets the node that's rebooting finish, then stops before the next one.
func (m *Manager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job == nil || m.job.State != Running {
		return errors.New("no update is running")
	}
	m.job.StopRequested = true
	m.saveLocked()
	return nil
}

// Continue reboots the nodes that are still staged after a stop.
func (m *Manager) Continue() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job == nil || m.job.State != Stopped || m.running {
		return errors.New("there's no stopped update to continue")
	}
	m.job.State, m.job.StopRequested, m.job.Error, m.job.FinishedAt = Running, false, "", 0
	m.running = true
	m.saveLocked()
	go m.run()
	return nil
}

// Dismiss forgets a finished or stopped job.
func (m *Manager) Dismiss() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.job != nil && (m.job.State == Running || m.running) {
		return errors.New("the update is still running; stop it first")
	}
	m.job = nil
	os.Remove(m.path)
	return nil
}

func (m *Manager) run() {
	defer func() {
		m.mu.Lock()
		m.running = false
		m.mu.Unlock()
	}()
	j := m.Get()

	// 1. Stage everywhere at once. Nodes already past this (on resume) are skipped.
	var wg sync.WaitGroup
	for _, n := range m.job.Nodes {
		if n.Step == StepWaiting || n.Step == StepStaging {
			wg.Add(1)
			go func() {
				defer wg.Done()
				m.stage(n)
			}()
		}
	}
	wg.Wait()

	// 2. Reboot one at a time, the main node last. A node already rebooting (on resume) is
	// followed rather than rebooted again.
	order := make([]*Node, 0, len(j.Nodes))
	for _, local := range []bool{false, true} {
		for _, n := range m.job.Nodes {
			if n.Local == local {
				order = append(order, n)
			}
		}
	}
	// A failed reboot stops the rollout, unless no node is left waiting for one.
	stopAfter := func(n *Node) bool {
		for _, o := range m.Get().Nodes {
			if o.Step == StepStaged {
				m.finish(Stopped, n.Name+" didn't come back on the update, so the rollout stopped. The nodes that are left have it staged.")
				return true
			}
		}
		return false
	}
	for _, n := range order {
		switch n.Step {
		case StepStaged:
			m.mu.Lock()
			stop := m.job.StopRequested
			m.mu.Unlock()
			if stop {
				m.finish(Stopped, "Stopped before rebooting "+n.Name+". The nodes that are left have the update staged.")
				return
			}
			if !m.reboot(n) && stopAfter(n) {
				return
			}
		case StepRebooting, StepVerifying:
			if !m.waitBack(n) && stopAfter(n) {
				return
			}
		}
	}

	state := Done
	for _, n := range m.Get().Nodes {
		if n.Step == StepFailed {
			state = Failed
		}
	}
	m.finish(state, "")
}

func (m *Manager) finish(state, msg string) {
	m.update(func(j *Job) {
		j.State, j.Error, j.FinishedAt = state, msg, time.Now().Unix()
	})
	log.Printf("rollout: update %s %s %s", m.job.ID, state, msg)
}

// stage starts the node's update run (unless it's already going) and waits until it's staged.
func (m *Manager) stage(n *Node) {
	if n.Step == StepWaiting {
		st, err := m.nodes.Status(n.ID)
		if err != nil {
			m.fail(n, "Not reachable: "+err.Error())
			return
		}
		if !st.Supported {
			m.fail(n, "Not an image-based node.")
			return
		}
		m.update(func(*Job) {
			if st.Job != nil {
				n.PrevJob = st.Job.ID
			}
			if st.Booted != nil {
				n.From = st.Booted.Version
			}
			n.To = st.Check.Version
		})
		m.setStep(n, StepStaging, "Starting…")
		if err := m.nodes.Stage(n.ID); err != nil {
			m.fail(n, "Couldn't start the update: "+err.Error())
			return
		}
	}

	start := time.Unix(n.StepAt, 0)
	for {
		time.Sleep(m.Poll)
		st, err := m.nodes.Status(n.ID)
		switch {
		case err != nil:
			m.setStep(n, StepStaging, "Not reachable, retrying…")
		case st.Job == nil || st.Job.ID == n.PrevJob:
			if time.Since(start) > m.StartTimeout && !st.Updating {
				m.fail(n, "The update didn't start on the node.")
				return
			}
		default:
			jb := st.Job
			m.update(func(*Job) {
				n.JobID = jb.ID
				if jb.To != "" {
					n.To = jb.To
				}
				if jb.ToName != "" {
					n.ToName = jb.ToName
				}
				if jb.From != "" {
					n.From = jb.From
				}
			})
			switch jb.Phase {
			case osupdate.PhaseStaged:
				m.update(func(*Job) { n.TargetChecksum = jb.TargetChecksum })
				m.setStep(n, StepStaged, "Waiting for its turn to reboot")
				return
			case osupdate.PhaseUpToDate:
				m.setStep(n, StepUpToDate, "Already up to date")
				return
			case osupdate.PhaseFailed:
				m.fail(n, orDefault(jb.Error, "The update failed; see the log."))
				return
			default:
				m.setStep(n, StepStaging, orDefault(st.Progress, "Downloading…"))
			}
		}
		if time.Since(start) > m.StageTimeout {
			m.fail(n, fmt.Sprintf("Still not staged after %s.", m.StageTimeout))
			return
		}
	}
}

// reboot reboots the node into its staged update and waits until it's back on it.
func (m *Manager) reboot(n *Node) bool {
	st, err := m.nodes.Status(n.ID)
	if err != nil {
		m.fail(n, "Not reachable before its reboot: "+err.Error())
		return false
	}
	m.update(func(*Job) { n.BootBefore, n.WentDown = st.BootID, false })
	m.setStep(n, StepRebooting, "Rebooting…")
	if err := m.nodes.Apply(n.ID); err != nil {
		m.fail(n, "Couldn't reboot into the update: "+err.Error())
		return false
	}
	return m.waitBack(n)
}

// waitBack waits until the node is up on a new boot and its update is checked.
func (m *Manager) waitBack(n *Node) bool {
	start := time.Unix(n.StepAt, 0)
	for {
		time.Sleep(m.Poll)
		st, err := m.nodes.Status(n.ID)
		switch {
		case err != nil:
			if !n.WentDown {
				m.update(func(*Job) { n.WentDown = true })
			}
			m.setStep(n, n.Step, "Offline while it reboots…")
		case st.BootID == n.BootBefore:
			if st.Job != nil && st.Job.Phase == osupdate.PhaseFailed {
				m.fail(n, orDefault(st.Job.Error, "The reboot into the update failed."))
				return false
			}
			m.setStep(n, StepRebooting, "Shutting down…")
		default:
			m.update(func(*Job) { n.WentDown = true })
			jb := st.Job
			booted := ""
			if st.Booted != nil {
				booted = st.Booted.Version
			}
			switch {
			case jb != nil && jb.Phase == osupdate.PhaseDone && (n.TargetChecksum == "" || jb.TargetChecksum == n.TargetChecksum):
				m.update(func(*Job) {
					if booted != "" {
						n.To = booted
					}
				})
				m.setStep(n, StepDone, "Running "+orDefault(jb.ToName, booted))
				return true
			case jb != nil && jb.Phase == osupdate.PhaseFailed:
				m.setStep(n, StepVerifying, "") // it's back; the image check failed
				m.fail(n, orDefault(jb.Error, "It came back on the old image."))
				return false
			default:
				m.setStep(n, StepVerifying, "Back online, checking the image…")
			}
		}
		if time.Since(start) > m.RebootTimeout {
			if n.WentDown {
				m.fail(n, fmt.Sprintf("Not back on the update %s after it started rebooting.", m.RebootTimeout))
			} else {
				m.fail(n, fmt.Sprintf("It didn't reboot within %s.", m.RebootTimeout))
			}
			return false
		}
	}
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}
