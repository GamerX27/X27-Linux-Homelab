package docker

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func waitStackJob(t *testing.T, s *StackJobs, name string) StackJob {
	t.Helper()
	for range 200 {
		if j, _ := s.Job(name); j != nil && !j.Running() {
			return *j
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("job never finished")
	return StackJob{}
}

func TestStackJob(t *testing.T) {
	s := NewStackJobs(New())
	release := make(chan struct{})
	j, err := s.start("web", "start", func() (string, error) {
		s.logLine("web", "pulling nginx")
		<-release
		s.logLine("web", "started")
		return "", nil
	})
	if err != nil || j.Phase != "running" || j.Stack != "web" || j.Action != "start" {
		t.Fatalf("start: %+v %v", j, err)
	}
	if _, err := s.start("web", "stop", func() (string, error) { return "", nil }); err != ErrStackBusy {
		t.Fatalf("second job on the same stack: %v", err)
	}
	// Another stack isn't held up.
	if _, err := s.start("db", "stop", func() (string, error) { return "", nil }); err != nil {
		t.Fatalf("other stack: %v", err)
	}
	for {
		if j, _ := s.Job("web"); j.LastLine == "pulling nginx" {
			break
		}
		time.Sleep(time.Millisecond)
	}
	if l := s.List(); len(l) != 2 || l[1].Stack != "web" || !l[1].Running() {
		t.Fatalf("list: %+v", l)
	}
	close(release)
	if done := waitStackJob(t, s, "web"); done.Phase != "done" || done.FinishedAt == 0 || done.Error != "" {
		t.Fatalf("done: %+v", done)
	}
	if _, text := s.Job("web"); text != "pulling nginx\nstarted" {
		t.Fatalf("log %q", text)
	}

	// A warning keeps it done; an error fails it with the line that says what went wrong.
	s.start("web", "start", func() (string, error) { return "app keeps restarting (exit code 1)", nil })
	if w := waitStackJob(t, s, "web"); w.Phase != "done" || w.Warning == "" {
		t.Fatalf("warning: %+v", w)
	}
	s.start("web", "start", func() (string, error) {
		return "", errors.New(" Container web-app-1  Creating\nError response from daemon: port is already allocated\n")
	})
	f := waitStackJob(t, s, "web")
	if _, text := s.Job("web"); f.Phase != "failed" || f.Error != "Error response from daemon: port is already allocated" || !strings.Contains(text, "Failed: Error response") {
		t.Fatalf("failed: %+v %q", f, text)
	}
	if _, text := s.Job("web"); strings.Contains(text, "pulling nginx") {
		t.Fatal("a new job keeps the old log")
	}
}

func TestStackJobUnknown(t *testing.T) {
	s := NewStackJobs(New())
	if _, err := s.Start(testOwner(t), "web", "explode", false); err == nil {
		t.Fatal("unknown action started")
	}
	if j, _ := s.Job("web"); j != nil {
		t.Fatal("job made for a refused action")
	}
}

func TestTrouble(t *testing.T) {
	up := ctState{Name: "app", Status: "running"}
	for _, c := range []struct {
		first, now ctState
		want       string
	}{
		{up, up, ""},
		{up, ctState{Name: "app", Status: "restarting", ExitCode: 1, RestartCount: 1}, "app keeps restarting (exit code 1)"},
		{up, ctState{Name: "app", Status: "running", RestartCount: 2}, "app keeps restarting"},
		{up, ctState{Name: "app", Status: "exited", ExitCode: 137}, "app stopped with exit code 137"},
		{up, ctState{Name: "init", Status: "exited"}, ""}, // a one-shot that finished
		{ctState{Name: "app", Status: "running", RestartCount: 5}, ctState{Name: "app", Status: "running", RestartCount: 5}, ""},
	} {
		if got := trouble(c.first, c.now); got != c.want {
			t.Errorf("%+v → %q, want %q", c.now, got, c.want)
		}
	}
	if got := stripTimestamps("2026-11-02T10:00:00.123456789Z boom\nplain\n"); got != "boom\nplain\n" {
		t.Errorf("timestamps: %q", got)
	}
}

func TestStackSteps(t *testing.T) {
	st, err := stackSteps("recreate")
	if err != nil || len(st) != 2 || st[0][0] != "pull" || strings.Join(st[1], " ") != "up -d --force-recreate --remove-orphans" {
		t.Fatalf("recreate: %v %v", st, err)
	}
	st, err = stackSteps("apply")
	if err != nil || len(st) != 1 || strings.Join(st[0], " ") != "up -d --force-recreate --remove-orphans" {
		t.Fatalf("apply: %v %v", st, err)
	}
	for _, a := range []string{"start", "stop", "resume", "restart", "remove"} {
		if _, err := stackSteps(a); err != nil {
			t.Error(a, err)
		}
	}
}
