package rollout

import (
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/osupdate"
)

// sim is a set of nodes whose updates move one step per status call.
type sim struct {
	mu      sync.Mutex
	nodes   map[string]*simNode
	reboots []string // in the order they happened
}

type simNode struct {
	boot, polls int
	job         *osupdate.Job
	down        int  // status calls left while it's rebooting
	failStage   bool // the download fails
	oldImage    bool // comes back on the old image
	upToDate    bool
}

func newSim(ids ...string) *sim {
	s := &sim{nodes: map[string]*simNode{}}
	for _, id := range ids {
		s.nodes[id] = &simNode{boot: 1, job: &osupdate.Job{ID: "old", Phase: osupdate.PhaseDone}}
	}
	return s
}

func (s *sim) Status(id string) (osupdate.Status, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[id]
	if n.down > 0 {
		n.down--
		if n.down == 0 {
			n.boot++
			if n.oldImage {
				n.job.Phase, n.job.Error = osupdate.PhaseFailed, "came back on the old image"
			} else {
				n.job.Phase = osupdate.PhaseDone
			}
		}
		return osupdate.Status{}, errors.New("offline")
	}
	if j := n.job; j.Phase == osupdate.PhaseDownloading {
		if n.polls++; n.polls >= 2 {
			switch {
			case n.failStage:
				j.Phase, j.Error = osupdate.PhaseFailed, "registry said no"
			case n.upToDate:
				j.Phase = osupdate.PhaseUpToDate
			default:
				j.Phase, j.TargetChecksum, j.To = osupdate.PhaseStaged, "new", "44.2"
			}
		}
	}
	j := *n.job
	return osupdate.Status{Supported: true, BootID: fmt.Sprint(n.boot), Job: &j,
		Booted: &osupdate.Deployment{Version: "44.1"}}, nil
}

func (s *sim) Stage(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.nodes[id].job = &osupdate.Job{ID: "new-" + id, Phase: osupdate.PhaseDownloading}
	return nil
}

func (s *sim) Apply(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.nodes[id]
	if n.job.Phase != osupdate.PhaseStaged {
		return errors.New("nothing staged")
	}
	n.job.Phase = osupdate.PhaseRebooting
	n.down = 2
	s.reboots = append(s.reboots, id)
	return nil
}

func newManager(t *testing.T, s *sim) *Manager {
	m := New(filepath.Join(t.TempDir(), "rollout.json"), s)
	m.Poll = time.Millisecond
	return m
}

func wait(t *testing.T, m *Manager) *Job {
	t.Helper()
	for i := 0; i < 5000; i++ {
		m.mu.Lock()
		running := m.running
		m.mu.Unlock()
		if !running {
			return m.Get()
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("rollout didn't finish")
	return nil
}

func steps(j *Job) map[string]string {
	out := map[string]string{}
	for _, n := range j.Nodes {
		out[n.ID] = n.Step
	}
	return out
}

func TestRolloutRebootsOneAtATimeMainLast(t *testing.T) {
	s := newSim("local", "a", "b")
	m := newManager(t, s)
	_, err := m.Start("root", []Target{{ID: "local", Local: true}, {ID: "a"}, {ID: "b"}})
	if err != nil {
		t.Fatal(err)
	}
	j := wait(t, m)
	if j.State != Done {
		t.Fatalf("state %s (%s), steps %v", j.State, j.Error, steps(j))
	}
	if fmt.Sprint(s.reboots) != "[a b local]" {
		t.Fatalf("reboot order %v", s.reboots)
	}
	for _, n := range j.Nodes {
		if n.Step != StepDone || !n.WentDown || n.To != "44.1" && n.To != "44.2" {
			t.Fatalf("node %+v", n)
		}
	}
}

func TestRolloutStopsWhenANodeComesBackOld(t *testing.T) {
	s := newSim("local", "a", "b")
	s.nodes["a"].oldImage = true
	m := newManager(t, s)
	m.Start("root", []Target{{ID: "a"}, {ID: "b"}, {ID: "local", Local: true}})
	j := wait(t, m)
	st := steps(j)
	if j.State != Stopped || st["a"] != StepFailed || st["b"] != StepStaged || st["local"] != StepStaged {
		t.Fatalf("state %s, steps %v", j.State, st)
	}
	if fmt.Sprint(s.reboots) != "[a]" {
		t.Fatalf("reboots %v", s.reboots)
	}

	// Continue reboots the rest.
	if err := m.Continue(); err != nil {
		t.Fatal(err)
	}
	j = wait(t, m)
	st = steps(j)
	if j.State != Failed || st["b"] != StepDone || st["local"] != StepDone {
		t.Fatalf("after continue: state %s, steps %v", j.State, st)
	}
}

func TestRolloutLastNodeFailingFinishes(t *testing.T) {
	s := newSim("a")
	s.nodes["a"].oldImage = true
	m := newManager(t, s)
	m.Start("root", []Target{{ID: "a"}})
	j := wait(t, m)
	if j.State != Failed || j.Error != "" || j.Nodes[0].FailedStep != StepVerifying {
		t.Fatalf("state %s (%s), node %+v", j.State, j.Error, j.Nodes[0])
	}
}

func TestRolloutSkipsNodesThatFailToStage(t *testing.T) {
	s := newSim("a", "b", "c")
	s.nodes["a"].failStage = true
	s.nodes["c"].upToDate = true
	m := newManager(t, s)
	m.Start("root", []Target{{ID: "a"}, {ID: "b"}, {ID: "c"}})
	j := wait(t, m)
	st := steps(j)
	if j.State != Failed || st["a"] != StepFailed || st["b"] != StepDone || st["c"] != StepUpToDate {
		t.Fatalf("state %s, steps %v", j.State, st)
	}
	if fmt.Sprint(s.reboots) != "[b]" {
		t.Fatalf("reboots %v", s.reboots)
	}
}

// The main node reboots itself: the job is saved as rebooting and picked up by the next
// process, which sees the new boot and finishes it.
func TestRolloutResumesAfterMainReboot(t *testing.T) {
	s := newSim("local")
	path := filepath.Join(t.TempDir(), "rollout.json")
	m := New(path, s)
	m.Poll = time.Millisecond
	m.Start("root", []Target{{ID: "local", Local: true}})
	wait(t, m)

	// Simulate a job saved mid-reboot by an earlier process.
	s.nodes["local"].job.Phase = osupdate.PhaseRebooting
	s.nodes["local"].down = 2
	m.update(func(j *Job) {
		j.State = Running
		n := j.Nodes[0]
		n.Step, n.BootBefore, n.StepAt = StepRebooting, "2", time.Now().Unix()
	})

	m2 := New(path, s)
	m2.Poll = time.Millisecond
	if j := m2.Get(); j == nil || j.Nodes[0].Step != StepRebooting {
		t.Fatalf("loaded %+v", j)
	}
	m2.Resume()
	j := wait(t, m2)
	if j.State != Done || j.Nodes[0].Step != StepDone {
		t.Fatalf("state %s, steps %v", j.State, steps(j))
	}
}

func TestOneAtATime(t *testing.T) {
	s := newSim("a")
	m := newManager(t, s)
	m.Poll = 50 * time.Millisecond
	m.Start("root", []Target{{ID: "a"}})
	if _, err := m.Start("root", []Target{{ID: "a"}}); err == nil {
		t.Fatal("second rollout started")
	}
	if err := m.Dismiss(); err == nil {
		t.Fatal("dismissed a running rollout")
	}
	wait(t, m)
	if err := m.Dismiss(); err != nil || m.Get() != nil {
		t.Fatal("dismiss", err)
	}
}
