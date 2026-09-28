// Package api serves the dashboard. Local is the per-node API (/api/v1 on a node, and
// /api/n/local on the main node); server.go puts login, the UI and node proxying around it.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/auth"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/features"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/osupdate"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/system"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/terminal"
)

type ctxKey struct{}

func withUser(r *http.Request, user string) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), ctxKey{}, user))
}

func userOf(r *http.Request) string {
	u, _ := r.Context().Value(ctxKey{}).(string)
	return u
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, err error) {
	writeJSON(w, status, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	if err := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20)).Decode(v); err != nil {
		return errors.New("bad request body")
	}
	return nil
}

// result answers an action: its output on success, or the error (with output) on failure.
func result(w http.ResponseWriter, r *http.Request, what, out string, err error) {
	if err != nil {
		log.Printf("%s: %s failed: %v", userOf(r), what, err)
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	log.Printf("%s: %s", userOf(r), what)
	writeJSON(w, http.StatusOK, map[string]string{"output": out})
}

type Local struct {
	Version  string
	Dev      bool
	Upgrader *websocket.Upgrader

	sampler *system.Sampler
	checker *osupdate.Checker
	docker  *docker.Client
	updater *docker.Updater

	osMu     sync.Mutex
	osCached osupdate.Status
	osAt     time.Time
}

func NewLocal(version string, dev bool, up *websocket.Upgrader) *Local {
	d := docker.New()
	l := &Local{Version: version, Dev: dev, Upgrader: up,
		sampler: system.NewSampler(), checker: osupdate.NewChecker(), docker: d, updater: docker.NewUpdater(d)}
	go func() {
		time.Sleep(time.Minute)
		for {
			l.updater.Check()
			time.Sleep(12 * time.Hour)
		}
	}()
	return l
}

// osStatus caches rpm-ostree status briefly: the main node polls every node's summary.
func (l *Local) osStatus(fresh bool) osupdate.Status {
	l.osMu.Lock()
	defer l.osMu.Unlock()
	if fresh || time.Since(l.osAt) > 30*time.Second {
		l.osCached, l.osAt = l.checker.Status(), time.Now()
	}
	return l.osCached
}

// Summary is what the main node shows for every node in its list.
type Summary struct {
	Info             system.Info `json:"info"`
	CPUPercent       float64     `json:"cpuPercent"`
	MemTotal         uint64      `json:"memTotal"`
	MemUsed          uint64      `json:"memUsed"`
	UptimeSec        float64     `json:"uptimeSec"`
	Version          string      `json:"version"`
	UpdateAvailable  bool        `json:"updateAvailable"`
	UpdateVersion    string      `json:"updateVersion"`
	UpdateStaged     bool        `json:"updateStaged"`
	ContainerUpdates int         `json:"containerUpdates"`
	Running          int         `json:"running"`
	Containers       int         `json:"containers"`
	DashboardVersion string      `json:"dashboardVersion"`
}

func (l *Local) summary() Summary {
	st := l.sampler.Stats()
	s := Summary{Info: system.GetInfo(), CPUPercent: st.CPUPercent, MemTotal: st.MemTotal,
		MemUsed: st.MemUsed, UptimeSec: st.UptimeSec, DashboardVersion: l.Version}
	os := l.osStatus(false)
	if os.Booted != nil {
		s.Version = os.Booted.Version
	}
	s.UpdateAvailable, s.UpdateVersion = os.Check.Available, os.Check.Version
	s.UpdateStaged = os.Staged != nil
	for _, i := range l.updater.Status().Images {
		if i.State == "update" {
			s.ContainerUpdates++
		}
	}
	if info, err := l.docker.Info(); err == nil {
		s.Running, s.Containers = info.ContainersRunning, info.Containers
	}
	return s
}

func (l *Local) Handler() http.Handler {
	mux := http.NewServeMux()
	ok := func(w http.ResponseWriter, v any, err error) {
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, v)
	}

	mux.HandleFunc("GET /summary", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, l.summary())
	})
	mux.HandleFunc("GET /overview", func(w http.ResponseWriter, r *http.Request) {
		resp := map[string]any{"info": system.GetInfo(), "stats": l.sampler.Stats(), "summary": l.summary()}
		if info, err := l.docker.Info(); err == nil {
			resp["docker"] = info
		} else {
			resp["dockerError"] = err.Error()
		}
		writeJSON(w, http.StatusOK, resp)
	})

	// OS image updates
	mux.HandleFunc("GET /os", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, l.osStatus(true))
	})
	mux.HandleFunc("POST /os/check", func(w http.ResponseWriter, r *http.Request) {
		result(w, r, "check for OS update", "Checking…", l.checker.CheckAsync())
	})
	mux.HandleFunc("POST /os/update", func(w http.ResponseWriter, r *http.Request) {
		result(w, r, "start OS update", "Update started. The node reboots if an update is installed.", osupdate.Update())
	})
	mux.HandleFunc("POST /os/rollback", func(w http.ResponseWriter, r *http.Request) {
		result(w, r, "roll back OS", "Rolled back. Rebooting…", osupdate.Rollback())
	})
	mux.HandleFunc("POST /power/{action}", func(w http.ResponseWriter, r *http.Request) {
		a := r.PathValue("action")
		result(w, r, "power "+a, "Going down…", system.Power(a))
	})

	// Docker
	mux.HandleFunc("GET /docker/containers", func(w http.ResponseWriter, r *http.Request) {
		cs, err := l.docker.Containers()
		ok(w, cs, err)
	})
	mux.HandleFunc("POST /docker/containers/{id}/{action}", func(w http.ResponseWriter, r *http.Request) {
		id, a := r.PathValue("id"), r.PathValue("action")
		if !docker.ValidID(id) {
			writeErr(w, http.StatusBadRequest, errors.New("bad container id"))
			return
		}
		result(w, r, "container "+a+" "+id, "", l.docker.ContainerAction(id, a))
	})
	mux.HandleFunc("GET /docker/containers/{id}/logs", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !docker.ValidID(id) {
			writeErr(w, http.StatusBadRequest, errors.New("bad container id"))
			return
		}
		tail, _ := strconv.Atoi(r.URL.Query().Get("tail"))
		if tail <= 0 || tail > 5000 {
			tail = 300
		}
		logs, err := l.docker.Logs(id, tail)
		ok(w, map[string]string{"logs": logs}, err)
	})
	mux.HandleFunc("GET /docker/containers/{id}/stats", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		if !docker.ValidID(id) {
			writeErr(w, http.StatusBadRequest, errors.New("bad container id"))
			return
		}
		s, err := l.docker.ContainerStats(id)
		ok(w, s, err)
	})
	mux.HandleFunc("GET /docker/images", func(w http.ResponseWriter, r *http.Request) {
		v, err := l.docker.Images()
		ok(w, v, err)
	})
	mux.HandleFunc("POST /docker/images/remove", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ ID string }
		if err := readJSON(r, &body); err != nil || !docker.ValidID(body.ID) {
			writeErr(w, http.StatusBadRequest, errors.New("bad image id"))
			return
		}
		result(w, r, "remove image "+body.ID, "", l.docker.RemoveImage(body.ID))
	})
	mux.HandleFunc("GET /docker/volumes", func(w http.ResponseWriter, r *http.Request) {
		v, err := l.docker.Volumes()
		ok(w, v, err)
	})
	mux.HandleFunc("GET /docker/networks", func(w http.ResponseWriter, r *http.Request) {
		v, err := l.docker.Networks()
		ok(w, v, err)
	})
	mux.HandleFunc("POST /docker/prune/{what}", func(w http.ResponseWriter, r *http.Request) {
		n, err := l.docker.Prune(r.PathValue("what"))
		result(w, r, "prune "+r.PathValue("what"), "Freed "+humanBytes(n)+".", err)
	})
	mux.HandleFunc("GET /docker/compose", func(w http.ResponseWriter, r *http.Request) {
		cs, err := l.docker.Containers()
		ok(w, docker.Projects(cs), err)
	})
	mux.HandleFunc("POST /docker/compose/{name}/{action}", func(w http.ResponseWriter, r *http.Request) {
		name, a := r.PathValue("name"), r.PathValue("action")
		out, err := l.docker.ComposeAction(name, a)
		result(w, r, "compose "+a+" "+name, out, err)
	})
	mux.HandleFunc("GET /docker/updates", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, l.updater.Status())
	})
	mux.HandleFunc("POST /docker/updates/check", func(w http.ResponseWriter, r *http.Request) {
		go l.updater.Check()
		result(w, r, "check container updates", "Checking…", nil)
	})
	mux.HandleFunc("POST /docker/updates/apply", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Image string }
		if err := readJSON(r, &body); err != nil || !docker.ValidID(body.Image) {
			writeErr(w, http.StatusBadRequest, errors.New("bad image"))
			return
		}
		out, err := l.updater.Apply(body.Image)
		result(w, r, "update image "+body.Image, out, err)
	})

	// Terminal
	mux.HandleFunc("GET /terminal", func(w http.ResponseWriter, r *http.Request) {
		// On a paired node the user comes from the main node; they need an account here too.
		if u := userOf(r); !l.Dev && !auth.InWheel(u) {
			writeErr(w, http.StatusForbidden, errors.New("there's no user "+u+" in the wheel group on this node"))
			return
		}
		terminal.Serve(w, r, l.Upgrader, userOf(r), l.Dev)
	})

	// Features
	mux.HandleFunc("GET /features", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, features.Get())
	})
	mux.HandleFunc("POST /features/autoupdate", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Off                      bool
			Freq, Weekday, Day, Time string
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if body.Off {
			out, err := features.AutoupdateOff()
			result(w, r, "autoupdate off", out, err)
			return
		}
		out, err := features.SetAutoupdate(body.Freq, body.Weekday, body.Day, body.Time)
		result(w, r, "autoupdate on "+body.Freq, out, err)
	})
	mux.HandleFunc("POST /features/gotify", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ URL, Token string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		out, err := features.SetGotify(body.URL, body.Token)
		result(w, r, "gotify set", out, err)
	})
	mux.HandleFunc("POST /features/gotify/test", func(w http.ResponseWriter, r *http.Request) {
		out, err := features.GotifyTest()
		result(w, r, "gotify test", out, err)
	})
	mux.HandleFunc("POST /features/gotify/off", func(w http.ResponseWriter, r *http.Request) {
		out, err := features.GotifyOff()
		result(w, r, "gotify off", out, err)
	})
	mux.HandleFunc("POST /features/services/{id}", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Enabled bool }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		id := r.PathValue("id")
		result(w, r, "service "+id+" enabled="+strconv.FormatBool(body.Enabled), "", features.SetService(id, body.Enabled))
	})

	// Settings
	mux.HandleFunc("GET /settings", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, system.GetSettings())
	})
	mux.HandleFunc("GET /settings/timezones", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, system.Timezones())
	})
	mux.HandleFunc("POST /settings/hostname", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Hostname string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		result(w, r, "hostname "+body.Hostname, "", system.SetHostname(body.Hostname))
	})
	mux.HandleFunc("POST /settings/timezone", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Timezone string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		result(w, r, "timezone "+body.Timezone, "", system.SetTimezone(body.Timezone))
	})
	mux.HandleFunc("POST /settings/ntp", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Enabled bool }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		result(w, r, "ntp "+strconv.FormatBool(body.Enabled), "", system.SetNTP(body.Enabled))
	})
	return mux
}

func humanBytes(n uint64) string {
	units := []string{"B", "KB", "MB", "GB", "TB"}
	f := float64(n)
	i := 0
	for f >= 1024 && i < len(units)-1 {
		f /= 1024
		i++
	}
	return strconv.FormatFloat(f, 'f', 1, 64) + " " + units[i]
}
