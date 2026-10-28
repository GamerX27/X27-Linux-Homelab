// Package api serves the dashboard. Local is the per-node API (/api/v1 on a node, and
// /api/n/local on the main node); server.go puts login, the UI and node proxying around it.
package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"mime"
	"net/http"
	"os/user"
	"path"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/auth"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/backup"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/channel"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/features"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/files"
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

// startedJob answers a request that started a container update in the background: the
// job, which the UI then follows through GET /docker/updates.
func startedJob(w http.ResponseWriter, r *http.Request, what string, job docker.UpdateJob, err error) {
	if err != nil {
		log.Printf("%s: %s failed: %v", userOf(r), what, err)
		writeErr(w, http.StatusConflict, err)
		return
	}
	log.Printf("%s: %s started", userOf(r), what)
	writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
}

type Local struct {
	Version     string
	Dev         bool
	Upgrader    *websocket.Upgrader
	Fingerprint string // the dashboard's certificate, for Settings

	sampler *system.Sampler
	checker *osupdate.Checker
	docker  *docker.Client
	updater *docker.Updater
	backups *backup.Manager
	files   files.Runner

	osMu     sync.Mutex
	osCached osupdate.Status
	osAt     time.Time

	auMu     sync.Mutex
	auCached features.Autoupdate
	auAt     time.Time

	chMu     sync.Mutex
	chCached channel.Status
	chAt     time.Time
}

func NewLocal(version string, dev bool, up *websocket.Upgrader) *Local {
	d := docker.New()
	l := &Local{Version: version, Dev: dev, Upgrader: up,
		sampler: system.NewSampler(), checker: osupdate.NewChecker(), docker: d, updater: docker.NewUpdater(d),
		backups: backup.New(d), files: files.Runner{Dev: dev}}
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

// osAction starts an update step and drops the cached status, so the next summary shows it.
func (l *Local) osAction(fn func() error) error {
	err := fn()
	l.osMu.Lock()
	l.osAt = time.Time{}
	l.osMu.Unlock()
	return err
}

// autoupdate caches the schedule like osStatus; changing it from Features drops the cache.
func (l *Local) autoupdate() features.Autoupdate {
	l.auMu.Lock()
	defer l.auMu.Unlock()
	if time.Since(l.auAt) > time.Minute {
		l.auCached, l.auAt = features.GetAutoupdate(), time.Now()
	}
	return l.auCached
}

func (l *Local) dropAutoupdate() {
	l.auMu.Lock()
	l.auAt = time.Time{}
	l.auMu.Unlock()
}

// Summary is what the main node shows for every node in its list.
type Summary struct {
	Info               system.Info `json:"info"`
	CPUPercent         float64     `json:"cpuPercent"`
	MemTotal           uint64      `json:"memTotal"`
	MemUsed            uint64      `json:"memUsed"`
	UptimeSec          float64     `json:"uptimeSec"`
	Version            string      `json:"version"`
	UpdateAvailable    bool        `json:"updateAvailable"`
	UpdateVersion      string      `json:"updateVersion"`
	UpdateStaged       bool        `json:"updateStaged"`
	Updating           bool        `json:"updating"` // an OS update is being installed
	UpdatePhase        string      `json:"updatePhase"`
	AutoUpdate         bool        `json:"autoUpdate"`     // the autoupdate timer installs updates on its own
	AutoUpdateNext     int64       `json:"autoUpdateNext"` // its next run, unix seconds
	BootID             string      `json:"bootId"`
	ContainerUpdates   int         `json:"containerUpdates"`
	ContainersUpdating bool        `json:"containersUpdating"` // a container update is running
	Running            int         `json:"running"`
	Containers         int         `json:"containers"`
	DashboardVersion   string      `json:"dashboardVersion"`
	Channel            string      `json:"channel"` // release channel, "" if unknown

	// The images behind ContainerUpdates, with the stacks and containers that use them.
	ContainerUpdateImages []docker.ImageUpdate `json:"containerUpdateImages,omitempty"`
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
	s.Updating = os.Updating
	s.BootID = os.BootID
	if os.Job != nil {
		s.UpdatePhase = os.Job.Phase
	}
	s.Channel = l.channelStatus(false).Current
	au := l.autoupdate()
	s.AutoUpdate, s.AutoUpdateNext = au.Enabled, au.NextRun
	ct := l.updater.Status()
	for _, i := range ct.Images {
		if i.State == "update" {
			s.ContainerUpdates++
			s.ContainerUpdateImages = append(s.ContainerUpdateImages, i)
		}
	}
	s.ContainersUpdating = ct.Job != nil && ct.Job.Running()
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
		result(w, r, "start OS update", "Update started. The node reboots if an update is installed.", l.osAction(osupdate.Update))
	})
	mux.HandleFunc("POST /os/stage", func(w http.ResponseWriter, r *http.Request) {
		result(w, r, "stage OS update", "Downloading the update.", l.osAction(osupdate.Stage))
	})
	mux.HandleFunc("POST /os/apply", func(w http.ResponseWriter, r *http.Request) {
		result(w, r, "reboot into OS update", "Rebooting into the update…", l.osAction(osupdate.Apply))
	})
	mux.HandleFunc("POST /os/cancel", func(w http.ResponseWriter, r *http.Request) {
		result(w, r, "cancel OS update", "Update cancelled.", l.osAction(osupdate.Cancel))
	})
	mux.HandleFunc("GET /os/log", func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.ParseInt(r.URL.Query().Get("offset"), 10, 64)
		text, next := osupdate.ReadLog(off)
		writeJSON(w, http.StatusOK, map[string]any{"text": text, "offset": next})
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
		if a == "recreate" {
			out, err := l.docker.Recreate(id)
			if err == nil {
				go l.updater.Check() // clear its "update available" badge
			}
			result(w, r, "pull and recreate container "+id, out, err)
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
	// Stacks: compose projects in the acting user's ~/docker, plus any others Docker runs.
	mux.HandleFunc("GET /docker/stacks", func(w http.ResponseWriter, r *http.Request) {
		o, err := l.owner(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		cs, err := l.docker.Containers()
		ok(w, map[string]any{"root": o.Root(), "stacks": docker.ListStacks(o, cs)}, err)
	})
	mux.HandleFunc("GET /docker/stacks/{name}", func(w http.ResponseWriter, r *http.Request) {
		o, err := l.owner(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		f, err := docker.ReadStack(o, r.PathValue("name"))
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, f)
	})
	saveStack := func(w http.ResponseWriter, r *http.Request, name string, create bool) {
		var body struct {
			Name, Compose, Env string
			Start              bool // start (create) or pull & recreate (edit) after saving
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if create {
			name = body.Name
		}
		o, err := l.owner(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		out, err := docker.SaveStack(o, name, body.Compose, body.Env, create)
		if err == nil && body.Start {
			action := "recreate"
			if create {
				action = "start"
			}
			var out2 string
			out2, err = l.docker.StackAction(o, name, action)
			out = strings.TrimSpace(out + "\n" + out2)
		}
		verb := "save"
		if create {
			verb = "create"
		}
		result(w, r, verb+" stack "+name, out, err)
	}
	mux.HandleFunc("POST /docker/stacks", func(w http.ResponseWriter, r *http.Request) {
		saveStack(w, r, "", true)
	})
	mux.HandleFunc("POST /docker/stacks/{name}", func(w http.ResponseWriter, r *http.Request) {
		saveStack(w, r, r.PathValue("name"), false)
	})
	mux.HandleFunc("POST /docker/stacks/{name}/{action}", func(w http.ResponseWriter, r *http.Request) {
		o, err := l.owner(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		name, a := r.PathValue("name"), r.PathValue("action")
		if a == "remove" {
			var body struct{ DeleteFolder bool }
			readJSON(r, &body) // an empty body means keep the folder
			out, err := l.docker.RemoveStack(o, name, body.DeleteFolder)
			what := "remove stack " + name
			if body.DeleteFolder {
				what += " and delete its folder"
			}
			result(w, r, what, out, err)
			return
		}
		if a == "update" {
			job, err := l.updater.StartUpdateStack(o, name)
			startedJob(w, r, "update stack "+name, job, err)
			return
		}
		out, err := l.docker.StackAction(o, name, a)
		if err == nil && a == "recreate" {
			go l.updater.Check() // clear its "update available" badge
		}
		result(w, r, "stack "+a+" "+name, out, err)
	})
	mux.HandleFunc("POST /docker/updates/apply-all", func(w http.ResponseWriter, r *http.Request) {
		job, err := l.updater.StartApplyAll()
		startedJob(w, r, "update all images", job, err)
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
		job, err := l.updater.StartApply(body.Image)
		startedJob(w, r, "update image "+body.Image, job, err)
	})
	mux.HandleFunc("GET /docker/updates/log", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"text": l.updater.Log()})
	})

	// Backups: stacks in ~/docker archived on a schedule or on demand (see internal/backup).
	mux.HandleFunc("GET /docker/backups", func(w http.ResponseWriter, r *http.Request) {
		o, err := l.owner(r)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		apps := []string{}
		cs, err := l.docker.Containers()
		for _, s := range docker.ListStacks(o, cs) {
			if s.InRoot {
				apps = append(apps, s.Name)
			}
		}
		ok(w, map[string]any{"backups": l.backups.Status(o), "apps": apps}, err)
	})
	mux.HandleFunc("POST /docker/backups/config", func(w http.ResponseWriter, r *http.Request) {
		var body backup.Config
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		o, err := l.owner(r)
		if err == nil {
			err = l.backups.Save(o, body)
		}
		result(w, r, "save backup settings", "", err)
	})
	mux.HandleFunc("POST /docker/backups/run", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Apps []string }
		readJSON(r, &body) // an empty body backs up the configured apps
		o, err := l.owner(r)
		var job backup.Job
		if err == nil {
			job, err = l.backups.Start(o, "manual", body.Apps)
		}
		what := "back up " + strings.Join(body.Apps, ", ")
		if len(body.Apps) == 0 {
			what = "back up the selected apps"
		}
		if err != nil {
			log.Printf("%s: %s failed: %v", userOf(r), what, err)
			writeErr(w, http.StatusConflict, err)
			return
		}
		log.Printf("%s: %s started", userOf(r), what)
		writeJSON(w, http.StatusAccepted, map[string]any{"job": job})
	})
	mux.HandleFunc("GET /docker/backups/log", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"text": l.backups.Log()})
	})
	mux.HandleFunc("POST /docker/backups/delete", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ App, File string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		o, err := l.owner(r)
		if err == nil {
			err = l.backups.Delete(o, body.App, body.File)
		}
		result(w, r, "delete backup "+body.File, "", err)
	})

	// Files: the acting user's home folder, with that user's permissions (see internal/files).
	fsUser := func(r *http.Request) string { return userOf(r) }
	mux.HandleFunc("GET /files/list", func(w http.ResponseWriter, r *http.Request) {
		var out json.RawMessage
		err := l.files.Run(fsUser(r), "list", files.Args{Path: r.URL.Query().Get("path")}, nil, &out)
		ok(w, out, err)
	})
	mux.HandleFunc("GET /files/read", func(w http.ResponseWriter, r *http.Request) {
		var out json.RawMessage
		err := l.files.Run(fsUser(r), "read", files.Args{Path: r.URL.Query().Get("path")}, nil, &out)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, out)
	})
	// fileOp answers {output}: for extract and compress, the path of what they made.
	fileOp := func(op string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			var body struct {
				Path, To, Content, Mode string
				Paths                   []string
				Create, Recursive       bool
			}
			// Editor saves can be up to files.MaxEdit, which JSON escaping can more than double.
			if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 3*files.MaxEdit)).Decode(&body); err != nil {
				writeErr(w, http.StatusBadRequest, errors.New("bad request body"))
				return
			}
			var stdin io.Reader
			if op == "write" {
				stdin = strings.NewReader(body.Content)
			}
			a := files.Args{Path: body.Path, Paths: body.Paths, To: body.To, Mode: body.Mode, Create: body.Create, Recursive: body.Recursive}
			var out struct{ Path string }
			err := l.files.Run(fsUser(r), op, a, stdin, &out)
			what := "files " + op + " ~/" + body.Path
			if len(body.Paths) > 0 {
				what = "files " + op + " ~/" + strings.Join(body.Paths, ", ~/")
			}
			if body.To != "" || (op == "move" || op == "copy") && len(body.Paths) > 0 {
				what += " -> ~/" + body.To
			}
			if body.Mode != "" {
				what += " " + body.Mode
			}
			result(w, r, what, out.Path, err)
		}
	}
	mux.HandleFunc("POST /files/write", fileOp("write"))
	mux.HandleFunc("POST /files/mkdir", fileOp("mkdir"))
	mux.HandleFunc("POST /files/move", fileOp("move"))
	mux.HandleFunc("POST /files/copy", fileOp("copy"))
	mux.HandleFunc("POST /files/delete", fileOp("delete"))
	mux.HandleFunc("POST /files/chmod", fileOp("chmod"))
	mux.HandleFunc("POST /files/extract", fileOp("extract"))
	mux.HandleFunc("POST /files/compress", fileOp("compress"))
	mux.HandleFunc("POST /files/upload", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		a := files.Args{Path: q.Get("path"), Name: q.Get("name"), Overwrite: q.Get("overwrite") == "1", Parents: q.Get("parents") == "1"}
		var out struct{ Size int64 }
		err := l.files.Run(fsUser(r), "upload", a, http.MaxBytesReader(w, r.Body, files.MaxUpload+1), &out)
		result(w, r, "files upload ~/"+path.Join(a.Path, a.Name), "", err)
	})
	// Download: one file as is, a folder as .tar.gz, several entries (?path=a&path=b) as
	// .zip. inline=1 shows media and PDFs in the browser instead (see previewType).
	mux.HandleFunc("GET /files/download", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		paths := q["path"]
		op, a, what := "download", files.Args{Path: q.Get("path")}, "~/"+q.Get("path")
		var name, ctype string
		if len(paths) > 1 {
			dir := path.Dir(strings.Trim(paths[0], "/"))
			name = path.Base(dir)
			if dir == "." {
				name = userOf(r)
			}
			op, a, what = "zip", files.Args{Paths: paths}, "~/"+strings.Join(paths, ", ~/")
			name, ctype = name+".zip", "application/zip"
		} else {
			var st files.Stat
			if err := l.files.Run(fsUser(r), "stat", a, nil, &st); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
			name, ctype = st.Name, "application/octet-stream"
			if st.IsDir {
				name, ctype = st.Name+".tar.gz", "application/gzip"
			} else {
				w.Header().Set("Content-Length", strconv.FormatInt(st.Size, 10))
			}
		}
		disposition := "attachment"
		if t := previewType(name); t != "" && q.Get("inline") == "1" && op == "download" {
			disposition, ctype = "inline", t
			w.Header().Set("X-Content-Type-Options", "nosniff")
			if t != "application/pdf" { // browsers refuse to show PDFs in a sandbox
				w.Header().Set("Content-Security-Policy", "sandbox")
			}
		}
		w.Header().Set("Content-Type", ctype)
		w.Header().Set("Content-Disposition", mime.FormatMediaType(disposition, map[string]string{"filename": name}))
		if disposition == "attachment" {
			log.Printf("%s: files download %s", userOf(r), what)
		}
		sw := &sentWriter{w: w}
		if err := l.files.Stream(r.Context(), fsUser(r), op, a, sw); err != nil {
			log.Printf("%s: download %s failed: %v", userOf(r), what, err)
			if !sw.sent {
				w.Header().Del("Content-Disposition")
				w.Header().Del("Content-Length")
				writeErr(w, http.StatusBadRequest, err)
			}
		}
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
		defer l.dropAutoupdate()
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
	mux.HandleFunc("GET /features/gotify/token", func(w http.ResponseWriter, r *http.Request) {
		token, err := features.GotifyToken()
		if err != nil {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		log.Printf("%s: gotify token shown", userOf(r))
		writeJSON(w, http.StatusOK, map[string]string{"token": token})
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
	l.adminRoutes(mux)
	l.storageRoutes(mux)
	return mux
}

// owner is whose ~/docker holds the stacks: the acting user, or in --dev whoever runs
// the server (the dev login accepts any name).
func (l *Local) owner(r *http.Request) (docker.Owner, error) {
	name := userOf(r)
	if l.Dev {
		if u, err := user.Current(); err == nil {
			name = u.Username
		}
	}
	return docker.LookupOwner(name)
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

// previewType is the content type a file may be shown inline with: images, audio, video
// and PDF only. SVG and HTML can carry script, so they're always downloaded.
func previewType(name string) string {
	ext := strings.ToLower(path.Ext(name))
	switch ext {
	case ".png", ".jpg", ".jpeg", ".gif", ".webp", ".avif", ".bmp", ".ico":
		if ext == ".jpg" {
			ext = ".jpeg"
		}
		if ext == ".ico" {
			return "image/x-icon"
		}
		return "image/" + ext[1:]
	case ".mp4", ".webm":
		return "video/" + ext[1:]
	case ".mp3":
		return "audio/mpeg"
	case ".ogg", ".wav", ".flac":
		return "audio/" + ext[1:]
	case ".m4a":
		return "audio/mp4"
	case ".pdf":
		return "application/pdf"
	}
	return ""
}

// sentWriter notes whether anything reached the client, so a failure before the first
// byte can still be answered with an error.
type sentWriter struct {
	w    io.Writer
	sent bool
}

func (s *sentWriter) Write(b []byte) (int, error) {
	if len(b) > 0 {
		s.sent = true
	}
	return s.w.Write(b)
}
