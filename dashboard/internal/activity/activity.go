// Package activity keeps a record of who changed what on this node: every action taken
// through the dashboard, a paired main node or an API token, and logins. It's a file of
// JSON lines in the state folder, rotated at MaxSize with one older file kept.
package activity

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	Time   int64  `json:"time"`          // unix milliseconds
	User   string `json:"user"`          // the person (or "" for something nobody did, like a refused login)
	Via    string `json:"via,omitempty"` // web, main node, token:<name>, …
	Action string `json:"action"`
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Node   string `json:"node,omitempty"` // added by the main node when it merges nodes' logs
}

type Query struct {
	User   string // exact user
	Text   string // in the action or error, any case
	Failed bool   // only what failed
	Before int64  // only entries before this time (ms), for paging
	Limit  int
}

const (
	MaxSize      = 5 << 20
	DefaultLimit = 200
	MaxLimit     = 1000
)

var (
	mu   sync.Mutex
	path string
)

// Init sets where the log is kept; until then Add does nothing.
func Init(file string) {
	mu.Lock()
	path = file
	mu.Unlock()
}

// Add records an entry, stamping its time.
func Add(e Entry) {
	mu.Lock()
	defer mu.Unlock()
	if path == "" {
		return
	}
	if e.Time == 0 {
		e.Time = time.Now().UnixMilli()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	if st, err := os.Stat(path); err == nil && st.Size()+int64(len(b)) > MaxSize {
		os.Rename(path, path+".1")
	}
	os.MkdirAll(filepath.Dir(path), 0o700)
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	f.Write(append(b, '\n'))
	f.Close()
}

// Read returns the entries matching q, newest first.
func Read(q Query) ([]Entry, error) {
	mu.Lock()
	p := path
	mu.Unlock()
	if q.Limit <= 0 {
		q.Limit = DefaultLimit
	}
	q.Limit = min(q.Limit, MaxLimit)
	text := strings.ToLower(q.Text)
	var all []Entry
	for _, f := range []string{p + ".1", p} {
		if p == "" {
			break
		}
		fh, err := os.Open(f)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return nil, err
		}
		sc := bufio.NewScanner(fh)
		sc.Buffer(make([]byte, 64<<10), 1<<20)
		for sc.Scan() {
			var e Entry
			if json.Unmarshal(sc.Bytes(), &e) != nil {
				continue
			}
			if q.Before > 0 && e.Time >= q.Before || q.User != "" && e.User != q.User || q.Failed && e.OK {
				continue
			}
			if text != "" && !strings.Contains(strings.ToLower(e.Action+" "+e.Error+" "+e.Via), text) {
				continue
			}
			all = append(all, e)
		}
		fh.Close()
	}
	// Newest first, and at most Limit.
	out := make([]Entry, 0, min(len(all), q.Limit))
	for i := len(all) - 1; i >= 0 && len(out) < q.Limit; i-- {
		out = append(out, all[i])
	}
	return out, nil
}
