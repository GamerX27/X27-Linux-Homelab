package api

import (
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/nodes"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/tlsutil"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/vip"
)

// vipRoutes are every node's half of the virtual IP: its status, its settings (the main node
// sets the standby's), and the standby receiving the main node's state.
func (l *Local) vipRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings/vip", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, vip.GetStatus())
	})
	mux.HandleFunc("POST /settings/vip", func(w http.ResponseWriter, r *http.Request) {
		var c vip.Config
		if err := readJSON(r, &c); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		old := vip.Load()
		c.MainName, c.SyncedAt = old.MainName, old.SyncedAt
		what := "turn off the virtual IP"
		if c.Enabled {
			what = fmt.Sprintf("set the virtual IP %s/%d (%s)", c.Address, c.Prefix, c.Role)
		}
		result(w, r, what, "", vip.Apply(c))
	})
	mux.HandleFunc("POST /vip/sync", func(w http.ResponseWriter, r *http.Request) {
		var b vip.Bundle
		if err := readJSONLimit(r, &b, 16<<20); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if err := vip.ReceiveBundle(b); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
}

// vipSync keeps the standby's copy of the main node's state fresh.
type vipSync struct {
	store *nodes.Store
	mu    sync.Mutex
	at    int64
	err   string
	kick  chan struct{}
}

func (s *vipSync) push() error {
	c := vip.Load()
	if !c.Enabled || c.Role != vip.RoleMain || c.Standby == "" {
		return nil
	}
	n, ok := s.store.Get(c.Standby)
	if !ok {
		return errors.New("the standby node isn't paired anymore")
	}
	host, _ := os.Hostname()
	b, err := vip.MakeBundle(s.store, n.ID, host)
	if err == nil {
		err = s.store.PostBody(n, "", "/vip/sync", 30*time.Second, b, nil)
	}
	s.mu.Lock()
	if err != nil {
		s.err = err.Error()
	} else {
		s.at, s.err = time.Now().Unix(), ""
	}
	s.mu.Unlock()
	return err
}

func (s *vipSync) loop() {
	t := time.NewTicker(time.Minute)
	for {
		select {
		case <-t.C:
		case <-s.kick:
		}
		if err := s.push(); err != nil {
			log.Printf("virtual IP: copying to the standby: %v", err)
		}
	}
}

// Kick copies to the standby soon (after nodes or tokens change).
func (s *vipSync) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}

// vipMainRoutes: setting the virtual IP up from the main node, for both ends.
func vipMainRoutes(api *http.ServeMux, local *Local, store *nodes.Store) *vipSync {
	sy := &vipSync{store: store, kick: make(chan struct{}, 1)}
	go sy.loop()
	sy.Kick()

	api.HandleFunc("GET /api/vip", func(w http.ResponseWriter, r *http.Request) {
		st := vip.GetStatus()
		resp := map[string]any{"local": st}
		sy.mu.Lock()
		resp["syncedAt"], resp["syncError"] = sy.at, sy.err
		sy.mu.Unlock()
		if st.Enabled && st.Standby != "" {
			if n, ok := store.Get(st.Standby); ok {
				var ss vip.Status
				if err := store.GetJSON(n, "/settings/vip", 8*time.Second, &ss); err != nil {
					resp["standbyError"] = err.Error()
				} else {
					resp["standby"] = ss
				}
				resp["standbyName"] = n.Name
			}
		}
		writeJSON(w, http.StatusOK, resp)
	})

	api.HandleFunc("POST /api/vip", func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Enabled bool
			Address string
			Prefix  int
			Standby string
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		old := vip.Load()
		cfg, _ := config.Load()

		// Turning it off: the standby first, so it doesn't take the address as the main lets go.
		if !body.Enabled {
			if n, ok := store.Get(old.Standby); ok {
				if err := store.PostBody(n, userOf(r), "/settings/vip", 30*time.Second, vip.Config{}, nil); err != nil {
					log.Printf("virtual IP: turning it off on %s: %v", n.Name, err)
				}
			}
			result(w, r, "turn off the virtual IP", "", vip.Apply(vip.Config{}))
			return
		}

		n, ok := store.Get(body.Standby)
		if !ok {
			writeErr(w, http.StatusBadRequest, errors.New("pick a paired node as the standby"))
			return
		}
		peerHost, peerPort, err := net.SplitHostPort(n.Address)
		if err != nil || net.ParseIP(peerHost).To4() == nil {
			writeErr(w, http.StatusBadRequest, fmt.Errorf("the standby must be paired by its IPv4 address (it's %s)", n.Address))
			return
		}
		iface, err := vip.FindInterface(body.Address, body.Prefix)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		pw, rid := old.Password, old.RouterID
		if pw == "" || rid == 0 {
			pw, rid = vip.NewSecret()
		}
		port, _ := strconv.Atoi(peerPort)
		mine := vip.Config{Enabled: true, Address: body.Address, Prefix: body.Prefix, Role: vip.RoleMain, Peer: peerHost,
			Standby: n.ID, RouterID: rid, Password: pw, Port: cfg.Port}
		theirs := vip.Config{Enabled: true, Address: body.Address, Prefix: body.Prefix, Role: vip.RoleStandby, Peer: iface.IP,
			RouterID: rid, Password: pw, Port: port}
		if err := vip.Validate(mine); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		what := fmt.Sprintf("set the virtual IP %s/%d with %s as standby", body.Address, body.Prefix, n.Name)
		// The standby first: it needs the main node's state before it can stand in.
		if err := store.PostBody(n, userOf(r), "/settings/vip", 60*time.Second, theirs, nil); err != nil {
			result(w, r, what, "", fmt.Errorf("%s: %w", n.Name, err))
			return
		}
		// A standby that changed: the old one lets go.
		if old.Standby != "" && old.Standby != n.ID {
			if on, ok := store.Get(old.Standby); ok {
				store.PostBody(on, userOf(r), "/settings/vip", 30*time.Second, vip.Config{}, nil)
			}
		}
		if err := vip.Apply(mine); err != nil {
			result(w, r, what, "", err)
			return
		}
		// The certificate must name the virtual IP; a new one means restarting to serve it.
		_, changed, err := tlsutil.EnsureIPs(filepath.Join(config.StateDir, "tls"), []string{body.Address})
		if err != nil {
			result(w, r, what, "", fmt.Errorf("certificate: %w", err))
			return
		}
		if err := sy.push(); err != nil {
			log.Printf("virtual IP: copying to %s: %v", n.Name, err)
		}
		out := ""
		if changed && !local.Dev {
			out = "restart"
			restartSelf()
		}
		result(w, r, what, out, nil)
	})
	return sy
}
