package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/activity"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/backup"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/migrate"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/nodes"
)

// moveRoutes are a node's half of moving a stack to another node; the main node drives them
// (see internal/migrate). The stream from /out goes straight into /in on the other node.
func (l *Local) moveRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /backup/move/plan", func(w http.ResponseWriter, r *http.Request) {
		o, err := l.owner(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		p, err := l.backups.PlanMove(o, r.URL.Query().Get("stack"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, p)
	})
	mux.HandleFunc("POST /backup/move/check", func(w http.ResponseWriter, r *http.Request) {
		o, err := l.owner(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		var p backup.MovePlan
		if err := readJSON(r, &p); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, l.backups.CheckMoveIn(o, p))
	})
	// The stack as a .tar.gz stream. With stop=1 it's stopped first and left stopped. A
	// failure halfway ends the stream without the gzip trailer, so the other side notices.
	mux.HandleFunc("GET /backup/move/out", func(w http.ResponseWriter, r *http.Request) {
		o, err := l.owner(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		stack := r.URL.Query().Get("stack")
		w.Header().Set("Content-Type", "application/gzip")
		sw := &sentWriter{w: w}
		err = l.backups.MoveOut(r.Context(), sw, o, stack, r.URL.Query().Get("stop") == "1")
		act(r, "move out stack "+stack, err)
		if err != nil && !sw.sent {
			w.Header().Del("Content-Type")
			writeErr(w, http.StatusBadRequest, err)
		}
	})
	// No size limit: CheckMoveIn made sure there's room before the copy started.
	mux.HandleFunc("POST /backup/move/in", func(w http.ResponseWriter, r *http.Request) {
		o, err := l.owner(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		man, err := l.backups.MoveIn(o, r.Body)
		act(r, "move in stack "+man.Stack+" from "+man.Host, err)
		if err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, backup.ErrRunning) {
				code = http.StatusConflict
			}
			writeErr(w, code, err)
			return
		}
		writeJSON(w, http.StatusOK, man)
	})
	mux.HandleFunc("POST /backup/move/abort", func(w http.ResponseWriter, r *http.Request) {
		o, err := l.owner(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		var body struct {
			Stack, Folder string
			Volumes       []string
		}
		if err := readJSON(r, &body); err != nil || !docker.ValidStackName(body.Stack) {
			writeErr(w, http.StatusBadRequest, errors.New("bad request body"))
			return
		}
		result(w, r, "undo moving in stack "+body.Stack, "", l.backups.AbortMoveIn(o, body.Stack, body.Folder, body.Volumes))
	})
	mux.HandleFunc("POST /backup/move/finish", func(w http.ResponseWriter, r *http.Request) {
		o, err := l.owner(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		var body struct {
			Stack, To string
			Remove    bool
			Volumes   []string
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&body); err != nil || !docker.ValidStackName(body.Stack) {
			writeErr(w, http.StatusBadRequest, errors.New("bad request body"))
			return
		}
		what := "mark stack " + body.Stack + " as moved to " + body.To
		if body.Remove {
			what = "remove stack " + body.Stack + " after moving it to " + body.To
		}
		result(w, r, what, "", l.backups.FinishMoveOut(o, body.Stack, body.To, body.Remove, body.Volumes))
	})
}

// moveNodes lets the move controller reach the nodes: the main node's own API in-process
// (streams through a pipe, so nothing is held in memory) and the others by token.
type moveNodes struct {
	localAPI http.Handler
	store    *nodes.Store
}

func (mn moveNodes) node(id string) (nodes.Node, error) {
	n, ok := mn.store.Get(id)
	if !ok {
		return n, errors.New("no such node")
	}
	return n, nil
}

func (mn moveNodes) Name(id string) (string, error) {
	if id == "local" {
		h, _ := os.Hostname()
		return h, nil
	}
	n, err := mn.node(id)
	return n.Name, err
}

func (mn moveNodes) Stream(ctx context.Context, who migrate.Who, id, method, path string, body io.Reader) (*http.Response, error) {
	if id != "local" {
		n, err := mn.node(id)
		if err != nil {
			return nil, err
		}
		ct := ""
		if body != nil {
			ct = "application/gzip"
		}
		return mn.store.Request(ctx, n, method, path, who.User, who.Via, body, ct)
	}
	req := httptest.NewRequestWithContext(ctx, method, path, body)
	req = withVia(withUser(req, who.User), who.Via)
	pr, pw := io.Pipe()
	rw := &pipeResponse{header: http.Header{}, pw: pw, ready: make(chan struct{})}
	go func() {
		mn.localAPI.ServeHTTP(rw, req)
		rw.WriteHeader(http.StatusOK) // nothing written at all
		pw.Close()
	}()
	select {
	case <-rw.ready:
	case <-ctx.Done():
		pr.Close()
		return nil, ctx.Err()
	}
	return &http.Response{StatusCode: rw.code, Status: http.StatusText(rw.code), Header: rw.header, Body: pr}, nil
}

func (mn moveNodes) JSON(who migrate.Who, id, method, path string, body, out any, timeout time.Duration) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var resp *http.Response
	var err error
	if id == "local" {
		req := httptest.NewRequestWithContext(ctx, method, path, rd)
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		mn.localAPI.ServeHTTP(rec, withVia(withUser(req, who.User), who.Via))
		resp = rec.Result()
	} else {
		n, nerr := mn.node(id)
		if nerr != nil {
			return nerr
		}
		ct := ""
		if body != nil {
			ct = "application/json"
		}
		if resp, err = mn.store.Request(ctx, n, method, path, who.User, who.Via, rd, ct); err != nil {
			return err
		}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// pipeResponse is a ResponseWriter whose body goes into a pipe; ready closes once the status
// is known.
type pipeResponse struct {
	header http.Header
	pw     *io.PipeWriter
	code   int
	ready  chan struct{}
	once   sync.Once
}

func (p *pipeResponse) Header() http.Header { return p.header }

func (p *pipeResponse) WriteHeader(code int) {
	p.once.Do(func() {
		p.code = code
		close(p.ready)
	})
}

func (p *pipeResponse) Write(b []byte) (int, error) {
	p.WriteHeader(http.StatusOK)
	return p.pw.Write(b)
}

func (p *pipeResponse) Flush() {}

// moveMainRoutes: moving stacks between nodes, from the main node.
func moveMainRoutes(api *http.ServeMux, localAPI http.Handler, store *nodes.Store) {
	mover := migrate.New(moveNodes{localAPI: localAPI, store: store})
	mover.OnDone = func(m migrate.Move) {
		what := fmt.Sprintf("move stack %s from %s to %s", m.Stack, m.FromName, m.ToName)
		var err error
		switch m.Phase {
		case "cancelled":
			what += " (cancelled)"
		case "failed":
			err = errors.New(m.Error)
		}
		if err != nil {
			log.Printf("%s: %s failed: %v", m.User, what, err)
		} else {
			log.Printf("%s: %s", m.User, what)
		}
		activity.Add(activity.Entry{User: m.User, Via: "web", Action: what, OK: err == nil, Error: errText(err)})
	}
	type req struct {
		From, Stack, To string
		DeleteAfter     bool
	}
	who := func(r *http.Request) migrate.Who { return migrate.Who{User: userOf(r), Via: viaOf(r)} }
	api.HandleFunc("POST /api/move/check", func(w http.ResponseWriter, r *http.Request) {
		var b req
		if err := readJSON(r, &b); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		ch, err := mover.Check(who(r), b.From, b.Stack, b.To)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, ch)
	})
	api.HandleFunc("POST /api/move", func(w http.ResponseWriter, r *http.Request) {
		var b req
		if err := readJSON(r, &b); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		m, err := mover.Start(who(r), b.From, b.Stack, b.To, b.DeleteAfter)
		what := "move stack " + b.Stack
		if err != nil {
			act(r, what, err)
			writeErr(w, http.StatusConflict, err)
			return
		}
		act(r, fmt.Sprintf("%s from %s to %s started", what, m.FromName, m.ToName), nil)
		writeJSON(w, http.StatusAccepted, map[string]any{"move": m})
	})
	api.HandleFunc("GET /api/move", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"moves": mover.List()})
	})
	api.HandleFunc("POST /api/move/{id}/cancel", func(w http.ResponseWriter, r *http.Request) {
		err := mover.Cancel(r.PathValue("id"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		act(r, "cancel a stack move", nil)
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
}
