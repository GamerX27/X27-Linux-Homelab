package files

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A Job is an extract or compress running in the background, so a big archive doesn't hold
// a request open and can be followed and cancelled from the Files page.
type Job struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`           // extract or compress
	Root       string     `json:"root,omitempty"` // the share it runs in, or "" for home
	Name       string     `json:"name"`           // what it works on, for the list
	Phase      string     `json:"phase"`          // running, done, failed or cancelled
	Done       int64      `json:"done"`
	Total      int64      `json:"total"` // -1 if unknown
	Output     string     `json:"output,omitempty"`
	Skipped    int        `json:"skipped,omitempty"`
	Renamed    int        `json:"renamed,omitempty"`
	Replaced   int        `json:"replaced,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"startedAt"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`

	user   string
	cancel context.CancelFunc
}

const (
	maxRunningJobs = 3
	keepJobs       = 15 * time.Minute
)

var ErrTooManyJobs = errors.New("three archive jobs are already running; wait for one to finish")

type Jobs struct {
	r      Runner
	mu     sync.Mutex
	jobs   map[string]*Job
	OnDone func(user string, j Job) // called when a job ends, for the log
}

func NewJobs(r Runner) *Jobs {
	return &Jobs{r: r, jobs: map[string]*Job{}}
}

// Start runs op ("extract" or "compress") as user in the background.
func (s *Jobs) Start(user, op string, a Args, name string) (Job, error) {
	if op != "extract" && op != "compress" {
		return Job{}, errors.New("only extract and compress run in the background")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	running := 0
	for _, j := range s.jobs {
		if j.user == user && j.Phase == "running" {
			running++
		}
	}
	if running >= maxRunningJobs {
		return Job{}, ErrTooManyJobs
	}
	b := make([]byte, 6)
	rand.Read(b)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	j := &Job{ID: hex.EncodeToString(b), Kind: op, Root: a.Root, Name: name, Phase: "running", Total: -1,
		StartedAt: time.Now(), user: user, cancel: cancel}
	s.jobs[j.ID] = j
	a.Progress = true
	go s.run(ctx, j, op, a)
	return *j, nil
}

func (s *Jobs) run(ctx context.Context, j *Job, op string, a Args) {
	defer j.cancel()
	var out ExtractResult // compress answers {path} too
	err := s.r.RunProgress(ctx, j.user, op, a, func(done, total int64) {
		s.mu.Lock()
		j.Done, j.Total = done, total
		s.mu.Unlock()
	}, &out)
	s.mu.Lock()
	now := time.Now()
	j.FinishedAt = &now
	switch {
	case ctx.Err() == context.Canceled:
		j.Phase = "cancelled"
	case err != nil:
		j.Phase, j.Error = "failed", err.Error()
		if ctx.Err() == context.DeadlineExceeded {
			j.Error = "it took more than 2 hours and was stopped"
		}
	default:
		j.Phase, j.Output = "done", out.Path
		j.Skipped, j.Renamed, j.Replaced = out.Skipped, out.Renamed, out.Replaced
		if j.Total > 0 {
			j.Done = j.Total
		}
	}
	done := *j
	s.mu.Unlock()
	if s.OnDone != nil {
		s.OnDone(j.user, done)
	}
}

// List returns the user's jobs, newest first.
func (s *Jobs) List(user string) []Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	out := []Job{}
	for _, j := range s.jobs {
		if j.user == user {
			out = append(out, *j)
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].StartedAt.After(out[b].StartedAt) })
	return out
}

// Cancel stops a running job; Dismiss forgets a finished one.
func (s *Jobs) Cancel(user, id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	j, ok := s.jobs[id]
	if !ok || j.user != user {
		return errors.New("no such job")
	}
	if j.Phase != "running" {
		delete(s.jobs, id)
		return nil
	}
	j.cancel()
	return nil
}

func (s *Jobs) prune() {
	for id, j := range s.jobs {
		if j.FinishedAt != nil && time.Since(*j.FinishedAt) > keepJobs {
			delete(s.jobs, id)
		}
	}
}

// RunProgress is Run for long operations: it can be cancelled through ctx (the fsop gets
// SIGTERM and cleans up), and reports the progress lines the fsop writes on stderr.
func (r Runner) RunProgress(ctx context.Context, username, op string, a Args, prog func(done, total int64), out any) error {
	cmd, err := r.command(ctx, username, op, a)
	if err != nil {
		return err
	}
	cmd.Cancel = func() error { return cmd.Process.Signal(syscall.SIGTERM) }
	cmd.WaitDelay = 15 * time.Second
	var stdout, errText bytes.Buffer
	cmd.Stdout = &stdout
	pr, pw := io.Pipe()
	cmd.Stderr = pw
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		sc := bufio.NewScanner(pr)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			line := sc.Text()
			if rest, ok := strings.CutPrefix(line, ProgressPrefix); ok {
				d, t, _ := strings.Cut(rest, " ")
				done, err1 := strconv.ParseInt(d, 10, 64)
				total, err2 := strconv.ParseInt(t, 10, 64)
				if err1 == nil && err2 == nil && prog != nil {
					prog(done, total)
				}
				continue
			}
			if errText.Len() > 0 {
				errText.WriteByte('\n')
			}
			errText.WriteString(line)
		}
		io.Copy(io.Discard, pr)
	}()
	err = cmd.Run()
	pw.Close()
	<-scanned
	if err != nil {
		return opError(&errText, err)
	}
	if out == nil {
		return nil
	}
	return json.Unmarshal(stdout.Bytes(), out)
}
