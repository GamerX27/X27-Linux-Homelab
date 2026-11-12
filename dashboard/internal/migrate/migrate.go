// Package migrate moves a compose stack, with its folder and named volumes, from one node
// to another. It runs on the main node: it streams the old node's /backup/move/out straight
// into the new node's /backup/move/in (nothing is stored on the main node), starts the stack
// there and checks it runs. If anything fails before that, the new node's copy is removed and
// the stack is started again where it was. The old copy is left stopped and marked as
// moved, or removed if asked.
package migrate

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/backup"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
)

// Who is acting, passed to each node: the person, and how (web, or token:<name>).
type Who struct{ User, Via string }

// Nodes reaches the nodes' APIs ("local" is the main node itself).
type Nodes interface {
	Name(id string) (string, error)
	JSON(who Who, id, method, path string, body, out any, timeout time.Duration) error
	Stream(ctx context.Context, who Who, id, method, path string, body io.Reader) (*http.Response, error)
}

type Move struct {
	ID          string   `json:"id"`
	Stack       string   `json:"stack"`
	From        string   `json:"from"`
	To          string   `json:"to"`
	FromName    string   `json:"fromName"`
	ToName      string   `json:"toName"`
	User        string   `json:"user"`
	DeleteAfter bool     `json:"deleteAfter"`
	Phase       string   `json:"phase"` // copying, starting, verifying, finishing, done, failed, cancelled
	Done        int64    `json:"done"`  // bytes copied
	Total       int64    `json:"total"` // about how many there are (the files' sizes; the stream is compressed)
	Warnings    []string `json:"warnings"`
	Error       string   `json:"error,omitempty"`
	FailedStep  string   `json:"failedStep,omitempty"` // copying or starting
	RolledBack  bool     `json:"rolledBack,omitempty"` // the stack runs on the old node again
	Log         []string `json:"log"`
	StartedAt   int64    `json:"startedAt"`
	FinishedAt  int64    `json:"finishedAt,omitempty"`

	who    Who
	cancel context.CancelFunc
}

func (m *Move) Running() bool { return m.FinishedAt == 0 }

// Check is what moving a stack would take and whether the new node can take it.
type Check struct {
	Plan     backup.MovePlan  `json:"plan"`
	Target   backup.MoveCheck `json:"target"`
	FromName string           `json:"fromName"`
	ToName   string           `json:"toName"`
	Problems []string         `json:"problems"` // the move can't start until these are fixed
	Warnings []string         `json:"warnings"`
}

type Controller struct {
	nodes  Nodes
	mu     sync.Mutex
	moves  []*Move
	OnDone func(m Move) // for the log
}

func New(n Nodes) *Controller { return &Controller{nodes: n} }

const keepMoves = 20

// verifyEvery is how often a moved stack's containers are checked after it starts.
var verifyEvery = 3 * time.Second

func (c *Controller) Check(who Who, from, stack, to string) (Check, error) {
	var ch Check
	if from == to {
		return ch, errors.New("pick another node to move it to")
	}
	if !docker.ValidStackName(stack) {
		return ch, errors.New("bad stack name")
	}
	var err error
	if ch.FromName, err = c.nodes.Name(from); err != nil {
		return ch, err
	}
	if ch.ToName, err = c.nodes.Name(to); err != nil {
		return ch, err
	}
	if err := c.nodes.JSON(who, from, "GET", "/backup/move/plan?stack="+url.QueryEscape(stack), nil, &ch.Plan, 2*time.Minute); err != nil {
		return ch, fmt.Errorf("%s: %w", ch.FromName, err)
	}
	ch.Problems, ch.Warnings = []string{}, slices.Clone(ch.Plan.Warnings)
	if err := c.nodes.JSON(who, to, "POST", "/backup/move/check", ch.Plan, &ch.Target, time.Minute); err != nil {
		ch.Problems = append(ch.Problems, fmt.Sprintf("%s can't take it: %v", ch.ToName, err))
		return ch, nil
	}
	for _, p := range ch.Target.Problems {
		ch.Problems = append(ch.Problems, ch.ToName+": "+p)
	}
	ch.Warnings = append(ch.Warnings, ch.Target.Warnings...)
	c.mu.Lock()
	for _, m := range c.moves {
		if m.Running() && m.Stack == stack && (m.From == from || m.To == from) {
			ch.Problems = append(ch.Problems, "this stack is already being moved")
		}
	}
	c.mu.Unlock()
	return ch, nil
}

// Start checks again and moves the stack in the background.
func (c *Controller) Start(who Who, from, stack, to string, deleteAfter bool) (Move, error) {
	ch, err := c.Check(who, from, stack, to)
	if err != nil {
		return Move{}, err
	}
	if len(ch.Problems) > 0 {
		return Move{}, errors.New(strings.Join(ch.Problems, "; "))
	}
	b := make([]byte, 6)
	rand.Read(b)
	ctx, cancel := context.WithCancel(context.Background())
	m := &Move{ID: hex.EncodeToString(b), Stack: stack, From: from, To: to, FromName: ch.FromName, ToName: ch.ToName,
		User: who.User, DeleteAfter: deleteAfter, Phase: "copying", Total: ch.Plan.Size, Warnings: slices.Clone(ch.Warnings),
		Log: []string{}, StartedAt: time.Now().Unix(), who: who, cancel: cancel}
	c.mu.Lock()
	c.moves = append(c.moves, m)
	for len(c.moves) > keepMoves {
		i := slices.IndexFunc(c.moves, func(x *Move) bool { return !x.Running() })
		if i < 0 {
			break
		}
		c.moves = slices.Delete(c.moves, i, i+1)
	}
	out := c.snapshot(m)
	c.mu.Unlock()
	go c.run(ctx, m, ch.Plan)
	return out, nil
}

func (c *Controller) snapshot(m *Move) Move {
	out := *m
	out.Log, out.Warnings = slices.Clone(m.Log), slices.Clone(m.Warnings)
	return out
}

// List returns the moves, newest first.
func (c *Controller) List() []Move {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Move, 0, len(c.moves))
	for i := len(c.moves) - 1; i >= 0; i-- {
		out = append(out, c.snapshot(c.moves[i]))
	}
	return out
}

// Cancel stops a move while it's copying; after that it runs to the end.
func (c *Controller) Cancel(id string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, m := range c.moves {
		if m.ID == id {
			if m.Phase != "copying" {
				return errors.New("it can only be cancelled while copying")
			}
			m.cancel()
			return nil
		}
	}
	return errors.New("no such move")
}

func (c *Controller) set(m *Move, fn func(m *Move)) {
	c.mu.Lock()
	fn(m)
	c.mu.Unlock()
}

func (c *Controller) logf(m *Move, format string, args ...any) {
	line := time.Now().Format("15:04:05") + " " + fmt.Sprintf(format, args...)
	c.set(m, func(m *Move) { m.Log = append(m.Log, line) })
}

// counter reports how much of the stream has gone through.
type counter struct {
	r  io.Reader
	fn func(n int64)
	n  int64
}

func (r *counter) Read(b []byte) (int, error) {
	n, err := r.r.Read(b)
	r.n += int64(n)
	r.fn(r.n)
	return n, err
}

func errorBody(resp *http.Response) error {
	var e struct{ Error string }
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
	if e.Error == "" {
		e.Error = resp.Status
	}
	return errors.New(e.Error)
}

func (c *Controller) run(ctx context.Context, m *Move, plan backup.MovePlan) {
	defer m.cancel()
	who := m.who
	stackPath := "/docker/stacks/" + url.PathEscape(m.Stack)
	c.logf(m, "Moving %s (%s) from %s to %s", m.Stack, human(plan.Size), m.FromName, m.ToName)
	for _, w := range m.Warnings {
		c.logf(m, "Note: %s", w)
	}

	// Start it again where it was, if it was running.
	rollback := func() {
		if !plan.Running {
			return
		}
		c.logf(m, "Starting %s again on %s", m.Stack, m.FromName)
		var out struct{ Output string }
		if err := c.nodes.JSON(who, m.From, "POST", stackPath+"/start", map[string]any{}, &out, 15*time.Minute); err != nil {
			c.logf(m, "Couldn't start it again on %s: %v", m.FromName, err)
			return
		}
		c.set(m, func(m *Move) { m.RolledBack = true })
	}
	fail := func(phase string, err error) {
		c.logf(m, "%s failed: %v", phase, err)
		c.set(m, func(m *Move) {
			m.Phase, m.Error, m.FailedStep = "failed", fmt.Sprintf("%s: %v", phase, err), strings.ToLower(phase)
			if ctx.Err() == context.Canceled {
				m.Phase, m.Error = "cancelled", ""
			}
		})
	}

	// Copy: the old node's stream straight into the new node.
	if plan.Running {
		c.logf(m, "Stopping %s on %s, then copying", m.Stack, m.FromName)
	} else {
		c.logf(m, "Copying")
	}
	src, err := c.nodes.Stream(ctx, who, m.From, "GET", "/backup/move/out?stop=1&stack="+url.QueryEscape(m.Stack), nil)
	if err != nil {
		fail("Copying", fmt.Errorf("%s: %w", m.FromName, err))
		rollback()
		c.finish(m)
		return
	}
	if src.StatusCode != http.StatusOK {
		err := errorBody(src)
		src.Body.Close()
		fail("Copying", fmt.Errorf("%s: %w", m.FromName, err))
		rollback()
		c.finish(m)
		return
	}
	body := &counter{r: src.Body, fn: func(n int64) { c.set(m, func(m *Move) { m.Done = n }) }}
	dst, err := c.nodes.Stream(ctx, who, m.To, "POST", "/backup/move/in", body)
	src.Body.Close()
	var man backup.MoveManifest
	if err == nil {
		if dst.StatusCode != http.StatusOK {
			err = errorBody(dst)
		} else {
			err = json.NewDecoder(dst.Body).Decode(&man)
		}
		dst.Body.Close()
	}
	if err != nil {
		fail("Copying", fmt.Errorf("%s: %w", m.ToName, err))
		rollback()
		c.finish(m)
		return
	}
	c.logf(m, "Copied %s to %s", human(body.n), m.ToName)
	volumes := make([]string, len(man.Volumes))
	for i, v := range man.Volumes {
		volumes[i] = v.Name
	}

	// Start it on the new node; if that fails, take the new copy away again.
	c.set(m, func(m *Move) { m.Phase = "starting" })
	c.logf(m, "Starting %s on %s", m.Stack, m.ToName)
	var out struct{ Output string }
	if err := c.nodes.JSON(who, m.To, "POST", stackPath+"/start", map[string]any{}, &out, 20*time.Minute); err != nil {
		for _, line := range lastLines(err.Error(), 8) {
			c.logf(m, "  %s", line)
		}
		fail("Starting", fmt.Errorf("%s: it didn't start", m.ToName))
		c.logf(m, "Removing the copy on %s", m.ToName)
		if err := c.nodes.JSON(who, m.To, "POST", "/backup/move/abort",
			map[string]any{"stack": m.Stack, "folder": man.Folder, "volumes": volumes}, nil, 5*time.Minute); err != nil {
			c.logf(m, "Couldn't remove it all: %v", err)
		}
		rollback()
		c.finish(m)
		return
	}

	// Check its containers stay up.
	c.set(m, func(m *Move) { m.Phase = "verifying" })
	c.logf(m, "Checking that its containers keep running")
	running, total := 0, 0
	for i := 0; i < 10; i++ {
		time.Sleep(verifyEvery)
		var st struct {
			Stacks []docker.Stack `json:"stacks"`
		}
		if err := c.nodes.JSON(who, m.To, "GET", "/docker/stacks", nil, &st, 30*time.Second); err != nil {
			continue
		}
		for _, s := range st.Stacks {
			if s.Name == m.Stack {
				running, total = s.Running, s.Total
			}
		}
		if i >= 3 && total > 0 && running == total {
			break
		}
	}
	if total == 0 || running < total {
		w := fmt.Sprintf("%s started, but %d of %d containers are running on %s; check its logs there", m.Stack, running, total, m.ToName)
		c.logf(m, "%s", w)
		c.set(m, func(m *Move) { m.Warnings = append(m.Warnings, w) })
	} else {
		c.logf(m, "All %d container%s running on %s", total, map[bool]string{true: "", false: "s"}[total == 1], m.ToName)
	}

	// The old copy: marked as moved, or removed.
	c.set(m, func(m *Move) { m.Phase = "finishing" })
	if m.DeleteAfter {
		c.logf(m, "Removing %s and its data from %s", m.Stack, m.FromName)
	}
	if err := c.nodes.JSON(who, m.From, "POST", "/backup/move/finish",
		map[string]any{"stack": m.Stack, "to": m.ToName, "remove": m.DeleteAfter, "volumes": volumes}, nil, 10*time.Minute); err != nil {
		w := fmt.Sprintf("the old copy on %s: %v", m.FromName, err)
		c.logf(m, "%s", w)
		c.set(m, func(m *Move) { m.Warnings = append(m.Warnings, w) })
	} else if !m.DeleteAfter {
		c.logf(m, "The old copy on %s is stopped and kept; remove it from there once you're happy", m.FromName)
	}
	c.set(m, func(m *Move) { m.Phase = "done" })
	c.logf(m, "Done: %s runs on %s", m.Stack, m.ToName)
	c.finish(m)
}

func (c *Controller) finish(m *Move) {
	c.set(m, func(m *Move) { m.FinishedAt = time.Now().Unix() })
	if c.OnDone != nil {
		c.mu.Lock()
		s := c.snapshot(m)
		c.mu.Unlock()
		c.OnDone(s)
	}
}

func lastLines(s string, n int) []string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return lines
}

func human(n int64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f, i := float64(n), 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	if i == 0 {
		return fmt.Sprintf("%d B", n)
	}
	return fmt.Sprintf("%.1f %s", f, units[i])
}
