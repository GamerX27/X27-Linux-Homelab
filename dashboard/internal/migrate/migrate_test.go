package migrate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/backup"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
)

// fakeNodes plays two nodes, a and b, recording the calls.
type fakeNodes struct {
	mu        sync.Mutex
	calls     []string
	startFail bool // starting on b fails
	received  []byte
}

func (f *fakeNodes) record(s string) {
	f.mu.Lock()
	f.calls = append(f.calls, s)
	f.mu.Unlock()
}

func (f *fakeNodes) Name(id string) (string, error) {
	if id == "a" || id == "b" {
		return "node-" + id, nil
	}
	return "", errors.New("no such node")
}

func (f *fakeNodes) JSON(_ Who, id, method, path string, body, out any, _ time.Duration) error {
	f.record(id + " " + method + " " + path)
	set := func(v any) { b, _ := json.Marshal(v); json.Unmarshal(b, out) }
	switch {
	case strings.HasPrefix(path, "/backup/move/plan"):
		set(backup.MovePlan{Stack: "web", Folder: "web", Running: true, Size: 5, Volumes: []backup.MoveVolume{{Name: "web_db"}}})
	case path == "/backup/move/check":
		set(backup.MoveCheck{Free: 1 << 30, Problems: []string{}})
	case id == "b" && path == "/docker/stacks/web/start" && f.startFail:
		return errors.New("port is already allocated")
	case path == "/docker/stacks":
		set(map[string]any{"stacks": []docker.Stack{{Name: "web", Running: 2, Total: 2}}})
	}
	return nil
}

func (f *fakeNodes) Stream(_ context.Context, _ Who, id, method, path string, body io.Reader) (*http.Response, error) {
	f.record(id + " " + method + " " + strings.Split(path, "?")[0])
	if method == "GET" {
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("stream"))}, nil
	}
	b, _ := io.ReadAll(body)
	f.mu.Lock()
	f.received = b
	f.mu.Unlock()
	man, _ := json.Marshal(backup.MoveManifest{Stack: "web", Folder: "web", Volumes: []backup.MoveVolume{{Name: "web_db"}}})
	return &http.Response{StatusCode: 200, Body: io.NopCloser(bytes.NewReader(man))}, nil
}

func init() { verifyEvery = time.Millisecond }

func wait(t *testing.T, c *Controller) Move {
	t.Helper()
	for i := 0; i < 400; i++ {
		if m := c.List()[0]; !m.Running() {
			return m
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("the move didn't finish")
	return Move{}
}

func has(calls []string, want string) bool {
	for _, c := range calls {
		if c == want {
			return true
		}
	}
	return false
}

func TestMove(t *testing.T) {
	f := &fakeNodes{}
	c := New(f)
	if _, err := c.Start(Who{User: "anna"}, "a", "web", "a", false); err == nil {
		t.Fatal("moved to the same node")
	}
	if _, err := c.Start(Who{User: "anna"}, "a", "web", "b", true); err != nil {
		t.Fatal(err)
	}
	m := wait(t, c)
	if m.Phase != "done" || string(f.received) != "stream" || m.Done != 6 {
		t.Fatalf("%+v", m)
	}
	if !has(f.calls, "b POST /docker/stacks/web/start") || !has(f.calls, "a POST /backup/move/finish") {
		t.Fatalf("calls %v", f.calls)
	}
}

func TestMoveRollsBackWhenItDoesntStart(t *testing.T) {
	f := &fakeNodes{startFail: true}
	c := New(f)
	if _, err := c.Start(Who{User: "anna"}, "a", "web", "b", false); err != nil {
		t.Fatal(err)
	}
	m := wait(t, c)
	if m.Phase != "failed" || !m.RolledBack {
		t.Fatalf("%+v", m)
	}
	if !has(f.calls, "b POST /backup/move/abort") || !has(f.calls, "a POST /docker/stacks/web/start") || has(f.calls, "a POST /backup/move/finish") {
		t.Fatalf("calls %v", f.calls)
	}
}
