package api

import (
	"errors"
	"net/http"
	"strconv"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/channel"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/features"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/logs"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/netbird"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/run"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/tlsutil"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/users"
)

// channelStatus caches `channel --json` (it runs rpm-ostree status) for the node summaries.
func (l *Local) channelStatus(fresh bool) channel.Status {
	l.chMu.Lock()
	defer l.chMu.Unlock()
	if fresh || time.Since(l.chAt) > 2*time.Minute {
		l.chCached, l.chAt = channel.Get(), time.Now()
	}
	return l.chCached
}

// restartSelf restarts dashboard.service after the reply has gone out.
func restartSelf() {
	go func() {
		time.Sleep(time.Second)
		run.Cmd(30*time.Second, "systemctl", "restart", "dashboard.service")
	}()
}

// adminRoutes: channel, NetBird, the dashboard's own settings, users and logs, the parts
// that used to need a shell on the node.
func (l *Local) adminRoutes(mux *http.ServeMux) {
	// Release channel
	mux.HandleFunc("GET /os/channel", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, l.channelStatus(true))
	})
	mux.HandleFunc("POST /os/channel", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Channel string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		err := l.osAction(func() error { return channel.Switch(body.Channel) })
		l.chMu.Lock()
		l.chAt = time.Time{}
		l.chMu.Unlock()
		result(w, r, "switch channel to "+body.Channel, "Switching to "+body.Channel+". It applies after a reboot.", err)
	})
	mux.HandleFunc("GET /os/channel/log", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, map[string]string{"text": channel.Log()})
	})

	// NetBird
	mux.HandleFunc("GET /netbird", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, netbird.Get())
	})
	mux.HandleFunc("POST /netbird/service", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Enabled bool }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		result(w, r, "netbird service enabled="+strconv.FormatBool(body.Enabled), "", features.SetService("netbird", body.Enabled))
	})
	mux.HandleFunc("POST /netbird/up", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ SetupKey, ManagementURL string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		out, err := netbird.Up(body.SetupKey, body.ManagementURL)
		result(w, r, "netbird up with a setup key", out, err)
	})
	mux.HandleFunc("POST /netbird/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ ManagementURL string }
		readJSON(r, &body) // an empty body uses the stored management server
		u, err := netbird.Login(body.ManagementURL)
		if err != nil {
			result(w, r, "netbird login", "", err)
			return
		}
		result(w, r, "netbird login", u, nil)
	})
	mux.HandleFunc("POST /netbird/down", func(w http.ResponseWriter, r *http.Request) {
		out, err := netbird.Down()
		result(w, r, "netbird down", out, err)
	})

	// The dashboard itself
	mux.HandleFunc("GET /dashboard", func(w http.ResponseWriter, r *http.Request) {
		cfg, err := config.Load()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		resp := map[string]any{"mode": cfg.Mode, "port": cfg.Port, "addresses": tlsutil.LocalIPs(),
			"fingerprint": l.Fingerprint, "version": l.Version, "dev": l.Dev}
		if cfg.Mode == config.ModeNode {
			resp["paired"] = cfg.TokenHash != ""
			resp["pairing"] = cfg.PairHash != ""
		} else {
			resp["presets"] = cfg.Presets
			resp["defaultPresets"] = config.DefaultPresetsRepo
		}
		writeJSON(w, http.StatusOK, resp)
	})
	mux.HandleFunc("POST /dashboard/port", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Port int }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if l.Dev {
			writeErr(w, http.StatusBadRequest, errors.New("the dev server takes its port from --port"))
			return
		}
		err := config.SetPort(body.Port)
		if err == nil {
			restartSelf()
		}
		result(w, r, "dashboard port "+strconv.Itoa(body.Port), "Restarting on port "+strconv.Itoa(body.Port)+".", err)
	})
	mux.HandleFunc("POST /dashboard/presets", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ URL string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		err := config.SetPresets(body.URL)
		if err == nil && !l.Dev {
			restartSelf()
		}
		result(w, r, "presets repository "+body.URL, "Saved. The dashboard restarts to load it.", err)
	})

	// Users
	mux.HandleFunc("GET /users", func(w http.ResponseWriter, r *http.Request) {
		list, err := users.Get()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"users": list.Users, "shells": list.Shells, "me": userOf(r)})
	})
	mux.HandleFunc("POST /users", func(w http.ResponseWriter, r *http.Request) {
		var body users.New
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		result(w, r, "add user "+body.Name, "", users.Add(body))
	})
	mux.HandleFunc("POST /users/{name}/{action}", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Group, Shell, Password string
			Member, Lock, RemoveHome bool
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		name, actor := r.PathValue("name"), userOf(r)
		var err error
		what := "user " + name + " "
		switch a := r.PathValue("action"); a {
		case "group":
			err = users.SetGroup(actor, name, body.Group, body.Member)
			what += "group " + body.Group + "=" + strconv.FormatBool(body.Member)
		case "shell":
			err = users.SetShell(name, body.Shell)
			what += "shell " + body.Shell
		case "password":
			err = users.SetPassword(name, body.Password)
			what += "password changed"
		case "lock":
			err = users.SetLocked(actor, name, body.Lock)
			what += "locked=" + strconv.FormatBool(body.Lock)
		case "delete":
			err = users.Delete(actor, name, body.RemoveHome)
			what += "deleted"
			if body.RemoveHome {
				what += " with home folder"
			}
		default:
			err = errors.New("unknown action " + a)
		}
		result(w, r, what, "", err)
	})

	// Logs
	mux.HandleFunc("GET /logs", func(w http.ResponseWriter, r *http.Request) {
		q := r.URL.Query()
		pr, err := strconv.Atoi(q.Get("priority"))
		if err != nil {
			pr = 7
		}
		limit, _ := strconv.Atoi(q.Get("limit"))
		res, err := logs.Read(logs.Query{Unit: q.Get("unit"), Kernel: q.Get("kernel") == "1", Priority: pr,
			Since: q.Get("since"), Grep: q.Get("grep"), Cursor: q.Get("cursor"), Limit: limit})
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, res)
	})
	mux.HandleFunc("GET /logs/units", func(w http.ResponseWriter, r *http.Request) {
		us, err := logs.Units()
		if err != nil {
			writeErr(w, http.StatusBadGateway, err)
			return
		}
		writeJSON(w, http.StatusOK, us)
	})
}
