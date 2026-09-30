package api

import (
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/nodes"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/osupdate"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/rollout"
)

// rolloutNodes lets the rollout reach the main node directly and the others by token.
type rolloutNodes struct {
	local *Local
	store *nodes.Store
}

func (rn rolloutNodes) node(id string) (nodes.Node, error) {
	n, ok := rn.store.Get(id)
	if !ok {
		return n, errors.New("the node was removed")
	}
	return n, nil
}

func (rn rolloutNodes) Status(id string) (osupdate.Status, error) {
	var st osupdate.Status
	if id == "local" {
		if osupdate.FakeDown() {
			return st, errors.New("rebooting")
		}
		return rn.local.osStatus(true), nil
	}
	n, err := rn.node(id)
	if err == nil {
		err = rn.store.GetJSON(n, "/os", 30*time.Second, &st)
	}
	return st, err
}

func (rn rolloutNodes) post(id, path string, fn func() error) error {
	if id == "local" {
		return rn.local.osAction(fn)
	}
	n, err := rn.node(id)
	if err == nil {
		err = rn.store.PostJSON(n, path, 30*time.Second, nil)
	}
	return err
}

func (rn rolloutNodes) Stage(id string) error { return rn.post(id, "/os/stage", osupdate.Stage) }
func (rn rolloutNodes) Apply(id string) error { return rn.post(id, "/os/apply", osupdate.Apply) }

// rolloutRoutes serves /api/rollout: the OS update the main node runs across nodes.
func rolloutRoutes(api *http.ServeMux, local *Local, store *nodes.Store) {
	m := rollout.New(filepath.Join(config.StateDir, "rollout.json"), rolloutNodes{local, store})
	m.Resume()

	api.HandleFunc("GET /api/rollout", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{"job": m.Get()})
	})
	api.HandleFunc("POST /api/rollout", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Nodes []string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		host, _ := os.Hostname()
		var targets []rollout.Target
		seen := map[string]bool{}
		for _, id := range body.Nodes {
			if seen[id] {
				continue
			}
			seen[id] = true
			if id == "local" {
				targets = append(targets, rollout.Target{ID: id, Name: host, Local: true})
				continue
			}
			n, ok := store.Get(id)
			if !ok {
				writeErr(w, http.StatusBadRequest, errors.New("no such node: "+id))
				return
			}
			targets = append(targets, rollout.Target{ID: id, Name: n.Name})
		}
		j, err := m.Start(userOf(r), targets)
		if err != nil {
			writeErr(w, http.StatusConflict, err)
			return
		}
		log.Printf("%s: started OS update %s on %d node(s)", userOf(r), j.ID, len(targets))
		writeJSON(w, http.StatusOK, map[string]any{"job": j})
	})
	api.HandleFunc("POST /api/rollout/stop", func(w http.ResponseWriter, r *http.Request) {
		result(w, r, "stop OS update", "Stops before the next reboot.", m.Stop())
	})
	api.HandleFunc("POST /api/rollout/continue", func(w http.ResponseWriter, r *http.Request) {
		result(w, r, "continue OS update", "Continuing.", m.Continue())
	})
	api.HandleFunc("DELETE /api/rollout", func(w http.ResponseWriter, r *http.Request) {
		result(w, r, "dismiss OS update", "", m.Dismiss())
	})
}

// fakeReboot makes the dev server look down while the fake OS update "reboots", so the UI's
// reconnect can be tried. Only the API: the page itself stays loadable.
func fakeReboot(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if (strings.HasPrefix(r.URL.Path, "/api/") || strings.HasPrefix(r.URL.Path, "/auth/")) && osupdate.FakeDown() {
			writeErr(w, http.StatusServiceUnavailable, errors.New("rebooting"))
			return
		}
		next.ServeHTTP(w, r)
	})
}
