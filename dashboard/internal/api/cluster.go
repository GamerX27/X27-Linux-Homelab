package api

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/backup"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/cluster"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/migrate"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/nodes"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/storage"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/tlsutil"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/vip"
)

// ---- Every node's side ------------------------------------------------------------------

// haGuard refuses to bring up an HA app on a node that doesn't hold the virtual IP: the
// cluster starts it on the one that does.
func haGuard(o docker.Owner, name string) error {
	if !docker.IsHA(o, name) || vip.Holds() {
		return nil
	}
	return errHAElsewhere
}

var errHAElsewhere = errors.New("HA app: it runs on the node that holds the virtual IP, and the dashboard starts it there (Cluster → High-availability apps)")

// haActions bring a stack up.
var haActions = []string{"start", "resume", "restart", "recreate", "apply", "update"}

// clusterRoutes are a node's half of the cluster: its status, HA app files, the switchover
// steps, and replicas in and out.
func (l *Local) clusterRoutes(mux *http.ServeMux) {
	w := cluster.Start(l.docker, l.backups)
	mux.HandleFunc("GET /cluster/local", func(rw http.ResponseWriter, r *http.Request) {
		writeJSON(rw, http.StatusOK, w.Status())
	})
	mux.HandleFunc("GET /docker/stacks/{name}/config", func(rw http.ResponseWriter, r *http.Request) {
		o, err := l.owner(r)
		if err != nil {
			writeErr(rw, http.StatusBadRequest, err)
			return
		}
		cf, err := docker.ComposeConfig(o, r.PathValue("name"))
		if err != nil {
			writeErr(rw, http.StatusBadRequest, err)
			return
		}
		dir, _ := docker.StackDir(o, r.PathValue("name"))
		writeJSON(rw, http.StatusOK, map[string]any{"config": cf, "dir": dir})
	})
	mux.HandleFunc("POST /docker/stacks/{name}/files", func(rw http.ResponseWriter, r *http.Request) {
		o, err := l.owner(r)
		if err != nil {
			writeErr(rw, http.StatusBadRequest, err)
			return
		}
		var f docker.HAFiles
		if err := readJSON(r, &f); err != nil {
			writeErr(rw, http.StatusBadRequest, err)
			return
		}
		out, err := docker.WriteHAFiles(o, r.PathValue("name"), f)
		result(rw, r, "write the HA app files of "+r.PathValue("name"), out, err)
	})
	mux.HandleFunc("POST /docker/stacks/{name}/ha-remove", func(rw http.ResponseWriter, r *http.Request) {
		o, err := l.owner(r)
		if err != nil {
			writeErr(rw, http.StatusBadRequest, err)
			return
		}
		result(rw, r, "make "+r.PathValue("name")+" an ordinary app again", "", docker.RemoveHAFiles(o, r.PathValue("name")))
	})
	mux.HandleFunc("POST /cluster/release", func(rw http.ResponseWriter, r *http.Request) {
		var body struct{ On bool }
		readJSON(r, &body)
		what := "compete for the virtual IP again"
		if body.On {
			what = "give up the virtual IP"
		}
		result(rw, r, what, "", vip.Release(body.On))
	})
	mux.HandleFunc("POST /cluster/handoff", func(rw http.ResponseWriter, r *http.Request) {
		var body struct{ To string }
		readJSON(r, &body)
		notes, err := w.HandOff(body.To)
		act(r, "stop the HA apps to hand the virtual IP over", err)
		if err != nil {
			writeErr(rw, http.StatusBadRequest, err)
			return
		}
		writeJSON(rw, http.StatusOK, map[string]any{"notes": notes})
	})
	mux.HandleFunc("POST /cluster/handoff/done", func(rw http.ResponseWriter, r *http.Request) {
		w.HandOffDone()
		writeJSON(rw, http.StatusOK, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /cluster/apps/{name}/push", func(rw http.ResponseWriter, r *http.Request) {
		var body struct{ Only []string }
		readJSON(r, &body)
		pushes, err := w.PushNow(r.PathValue("name"), body.Only)
		if err != nil {
			act(r, "copy HA app "+r.PathValue("name")+" to the other members", err)
			writeErr(rw, http.StatusConflict, err)
			return
		}
		var failed []string
		for _, p := range pushes {
			if !p.OK {
				failed = append(failed, p.Name+": "+p.Error)
			}
		}
		var err2 error
		if len(failed) > 0 {
			err2 = errors.New(strings.Join(failed, "; "))
		}
		act(r, "copy HA app "+r.PathValue("name")+" to the other members", err2)
		writeJSON(rw, http.StatusOK, map[string]any{"pushes": pushes})
	})
	// Replicas: only of HA apps, for their owner.
	haApp := func(r *http.Request) (docker.Owner, cluster.HAApp, error) {
		a, ok := cluster.Load().App(r.URL.Query().Get("stack"))
		if !ok {
			return docker.Owner{}, a, errors.New("no such HA app")
		}
		if a.Data != cluster.DataReplicated {
			return docker.Owner{}, a, errors.New("that HA app isn't replicated")
		}
		if !l.Dev && userOf(r) != a.Owner {
			return docker.Owner{}, a, errors.New("the app belongs to " + a.Owner)
		}
		o, err := l.owner(r)
		return o, a, err
	}
	mux.HandleFunc("GET /cluster/snapshot/out", func(rw http.ResponseWriter, r *http.Request) {
		o, a, err := haApp(r)
		if err != nil {
			writeErr(rw, http.StatusBadRequest, err)
			return
		}
		rw.Header().Set("Content-Type", "application/gzip")
		sw := &sentWriter{w: rw}
		err = l.backups.SnapshotOut(r.Context(), sw, o, a.Name)
		if err != nil {
			log.Printf("cluster: copy of %s out: %v", a.Name, err)
			if !sw.sent {
				rw.Header().Del("Content-Type")
				writeErr(rw, http.StatusConflict, err)
			}
		}
	})
	mux.HandleFunc("POST /cluster/snapshot/in", func(rw http.ResponseWriter, r *http.Request) {
		o, a, err := haApp(r)
		if err != nil {
			writeErr(rw, http.StatusBadRequest, err)
			return
		}
		cr := &countingReader{r: r.Body}
		man, err := l.backups.SnapshotIn(o, cr)
		if err == nil && man.Stack != a.Name {
			err = errors.New("the copy is of another app")
		}
		if err != nil {
			code := http.StatusBadRequest
			if errors.Is(err, backup.ErrAppRunning) || errors.Is(err, backup.ErrRunning) {
				code = http.StatusConflict
			}
			log.Printf("cluster: copy of %s in: %v", a.Name, err)
			writeErr(rw, code, err)
			return
		}
		w.Received(a.Name, man, cr.n)
		writeJSON(rw, http.StatusOK, man)
	})
}

type countingReader struct {
	r interface{ Read([]byte) (int, error) }
	n int64
}

func (c *countingReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}

// clusterPeer serves the main node's cluster API to the other members, who present the
// cluster secret: their status checks and replicas, nothing else.
func clusterPeer(localAPI http.Handler) http.Handler {
	allowed := []string{"/cluster/local", "/cluster/snapshot/out", "/cluster/snapshot/in"}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		sec := cluster.Load().Secret
		if !ok || sec == "" || subtle.ConstantTimeCompare([]byte(tok), []byte(sec)) != 1 {
			writeErr(w, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}
		p := strings.TrimPrefix(r.URL.Path, "/api/v1")
		if !slices.Contains(allowed, p) {
			writeErr(w, http.StatusNotFound, errors.New("not found"))
			return
		}
		http.StripPrefix("/api/v1", localAPI).ServeHTTP(w, withVia(withUser(r, r.Header.Get(nodes.UserHeader)), "cluster"))
	})
}

// ---- The main node's side ---------------------------------------------------------------

// clusterNodes is how the cluster code reaches every node, as the move does.
type clusterNodes struct{ moveNodes }

func (c clusterNodes) List() []cluster.NodeInfo {
	host, _ := os.Hostname()
	out := []cluster.NodeInfo{{ID: cluster.LocalID, Name: host}}
	for _, n := range c.store.List() {
		out = append(out, cluster.NodeInfo{ID: n.ID, Name: n.Name})
	}
	return out
}

func (c clusterNodes) JSON(user, id, method, path string, body, out any, timeout time.Duration) error {
	return c.moveNodes.JSON(migrate.Who{User: user, Via: "cluster"}, id, method, path, body, out, timeout)
}

type clusterMain struct {
	nodes    clusterNodes
	local    *Local
	shares   *cluster.Shares
	sync     *vipSync
	readOnly bool // a member standing in: it shows the cluster, the main node changes it
	switchMu sync.Mutex
	appMu    sync.Mutex
}

// Kick runs what follows a change of nodes: shares on a new node, fresh copies to members.
func (c *clusterMain) Kick() {
	if c == nil || c.readOnly {
		return
	}
	c.shares.Kick()
	c.sync.Kick()
}

func (c *clusterMain) name(id string) string {
	if c.readOnly {
		// A stand-in: "local" in the settings is the main node, and this node is its own id.
		v := vip.Load()
		switch id {
		case cluster.LocalID:
			return v.MainName
		case v.Self:
			id = cluster.LocalID
		}
	}
	if id == cluster.LocalID {
		h, _ := os.Hostname()
		return h
	}
	if n, ok := c.nodes.store.Get(id); ok {
		return n.Name
	}
	return id
}

// memberView is one member of the virtual IP, as the Cluster page shows it.
type memberView struct {
	NodeID   string               `json:"nodeId"`
	Name     string               `json:"name"`
	Priority int                  `json:"priority"`
	Online   bool                 `json:"online"`
	Error    string               `json:"error,omitempty"`
	VIP      *vip.Status          `json:"vip,omitempty"`
	Local    *cluster.LocalStatus `json:"local,omitempty"`
	Sync     *memberSync          `json:"sync,omitempty"`
}

// members asks every member for its virtual IP and HA app status.
func (c *clusterMain) members(st cluster.State) ([]memberView, string) {
	self := ""
	if c.readOnly {
		self = vip.Load().Self
	}
	out := make([]memberView, len(st.VIP.Members))
	var syncs map[string]memberSync
	if c.sync != nil {
		syncs = c.sync.status()
	}
	var wg sync.WaitGroup
	for i, m := range st.VIP.Members {
		out[i] = memberView{NodeID: m.NodeID, Name: c.name(m.NodeID), Priority: m.Priority}
		if s, ok := syncs[m.NodeID]; ok {
			out[i].Sync = &s
		}
		id := cluster.Self(self, m.NodeID)
		if id == cluster.MainPeer {
			out[i].Name = vip.Load().MainName
			out[i].Error = "the main node; this stand-in doesn't ask it"
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var vs vip.Status
			var ls cluster.LocalStatus
			if err := c.nodes.JSON("", id, "GET", "/settings/vip", nil, &vs, 6*time.Second); err != nil {
				out[i].Error = err.Error()
				return
			}
			out[i].Online, out[i].VIP = true, &vs
			if err := c.nodes.JSON("", id, "GET", "/cluster/local", nil, &ls, 6*time.Second); err == nil {
				out[i].Local = &ls
			}
		}()
	}
	wg.Wait()
	holder := ""
	for _, m := range out {
		if m.VIP != nil && m.VIP.Holds {
			holder = m.NodeID
		}
	}
	return out, holder
}

// nodeAddr is a member's own IPv4 address on the virtual IP's subnet and its dashboard port.
func (c *clusterMain) nodeAddr(id string, v cluster.VIPConfig) (string, int, error) {
	if id == cluster.LocalID {
		cfg, _ := config.Load()
		iface, err := vip.FindInterface(v.Address, v.Prefix)
		if err != nil {
			if vip.Fake {
				return "127.0.0.1", cfg.Port, nil
			}
			return "", 0, fmt.Errorf("%s: %w", c.name(id), err)
		}
		return iface.IP, cfg.Port, nil
	}
	n, ok := c.nodes.store.Get(id)
	if !ok {
		return "", 0, fmt.Errorf("node %s isn't paired", id)
	}
	host, port, err := net.SplitHostPort(n.Address)
	if err != nil || net.ParseIP(host).To4() == nil {
		return "", 0, fmt.Errorf("%s must be paired by its IPv4 address to be a member (it's %s)", n.Name, n.Address)
	}
	p, _ := strconv.Atoi(port)
	return host, p, nil
}

// migrateVIP turns an old main/standby virtual IP into the cluster's.
func migrateVIP() {
	v := vip.Load()
	st := cluster.Load()
	if !v.Enabled || v.Role != vip.RoleMain || v.Standby == "" || st.VIP.Address != "" {
		return
	}
	_, err := cluster.Update(func(s *cluster.State) error {
		s.VIP = cluster.VIPConfig{Enabled: true, Address: v.Address, Prefix: v.Prefix, RouterID: v.RouterID, Password: v.Password,
			Failback: true, Members: []cluster.Member{{NodeID: cluster.LocalID, Priority: 150}, {NodeID: v.Standby, Priority: 100}}}
		return nil
	})
	if err != nil {
		log.Printf("cluster: moving the virtual IP's settings over: %v", err)
		return
	}
	log.Printf("cluster: the virtual IP %s is a cluster setting now (Cluster → Virtual IP)", v.Address)
}

func errList(errs []string) error {
	if len(errs) == 0 {
		return nil
	}
	return errors.New(strings.Join(errs, "; "))
}

// clusterMainRoutes: the Cluster page's API, on the main node (and, to look only, on a member
// standing in for it).
func clusterMainRoutes(api *http.ServeMux, local *Local, localAPI http.Handler, store *nodes.Store, readOnly bool) *clusterMain {
	c := &clusterMain{nodes: clusterNodes{moveNodes{localAPI: localAPI, store: store}}, local: local, readOnly: readOnly}
	if !readOnly {
		migrateVIP()
		c.sync = newVIPSync(store)
		c.shares = cluster.NewShares(c.nodes)
		go c.shares.Loop()
		go c.appLoop()
	}
	refuse := func(w http.ResponseWriter) bool {
		if c.readOnly {
			writeErr(w, http.StatusConflict, errors.New("this node is standing in for the main node; change the cluster settings on the main node"))
		}
		return c.readOnly
	}

	api.HandleFunc("GET /api/cluster", func(w http.ResponseWriter, r *http.Request) {
		st := cluster.Load()
		members, holder := c.members(st)
		resp := map[string]any{"state": st.Public(), "members": members, "holder": holder, "readOnly": c.readOnly, "nodes": c.nodes.List()}
		if c.shares != nil {
			s, at := c.shares.Status()
			resp["shares"], resp["sharesAt"] = s, at
		} else {
			resp["shares"] = map[string]any{}
		}
		writeJSON(w, http.StatusOK, resp)
	})

	api.HandleFunc("POST /api/cluster/vip", func(w http.ResponseWriter, r *http.Request) {
		if refuse(w) {
			return
		}
		var body struct {
			Enabled  bool
			Address  string
			Prefix   int
			Members  []string
			Failback bool
		}
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		out, err := c.setVIP(r, body.Enabled, body.Address, body.Prefix, body.Members, body.Failback)
		what := "turn off the virtual IP"
		if body.Enabled {
			names := make([]string, len(body.Members))
			for i, id := range body.Members {
				names[i] = c.name(id)
			}
			what = fmt.Sprintf("set the virtual IP %s/%d on %s", body.Address, body.Prefix, strings.Join(names, ", "))
		}
		result(w, r, what, out, err)
	})

	type shareBody struct {
		storage.Mount
		Nodes    []string `json:"nodes"`
		Password string   `json:"password"`
	}
	saveShare := func(w http.ResponseWriter, r *http.Request, name string, create bool) {
		if refuse(w) {
			return
		}
		var body shareBody
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		if !create {
			body.Name = name
		}
		what := "add cluster share " + body.Name
		if !create {
			what = "change cluster share " + body.Name
		}
		err := c.saveShare(r, body.Mount, body.Nodes, body.Password, create)
		if err != nil {
			result(w, r, what, "", err)
			return
		}
		c.shares.Once()
		c.sync.Kick()
		st, _ := c.shares.Status()
		act(r, what, nil)
		writeJSON(w, http.StatusOK, map[string]any{"nodes": st[body.Name]})
	}
	api.HandleFunc("POST /api/cluster/shares", func(w http.ResponseWriter, r *http.Request) { saveShare(w, r, "", true) })
	api.HandleFunc("POST /api/cluster/shares/{name}", func(w http.ResponseWriter, r *http.Request) {
		saveShare(w, r, r.PathValue("name"), false)
	})
	api.HandleFunc("POST /api/cluster/shares/{name}/retry", func(w http.ResponseWriter, r *http.Request) {
		if refuse(w) {
			return
		}
		c.shares.Once()
		st, _ := c.shares.Status()
		writeJSON(w, http.StatusOK, map[string]any{"nodes": st[r.PathValue("name")]})
	})
	api.HandleFunc("DELETE /api/cluster/shares/{name}", func(w http.ResponseWriter, r *http.Request) {
		if refuse(w) {
			return
		}
		name := r.PathValue("name")
		_, err := cluster.Update(func(s *cluster.State) error {
			for _, a := range s.Apps {
				if slices.Contains(a.Shares, name) {
					return fmt.Errorf("the HA app %s keeps its data on %s; make it an ordinary app first", a.Name, name)
				}
			}
			i := slices.IndexFunc(s.Shares, func(sh cluster.Share) bool { return sh.Name == name })
			if i < 0 {
				return errors.New("no such cluster share")
			}
			s.Shares = slices.Delete(s.Shares, i, i+1)
			return nil
		})
		if err == nil {
			c.shares.Once()
			c.sync.Kick()
		}
		result(w, r, "remove cluster share "+name, "", err)
	})

	type appBody struct {
		Name     string `json:"name"`
		Node     string `json:"node"`
		Data     string `json:"data"`
		Interval int    `json:"interval"`
		VIPOnly  bool   `json:"vipOnly"`
	}
	api.HandleFunc("POST /api/cluster/apps/check", func(w http.ResponseWriter, r *http.Request) {
		var body appBody
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		chk, err := c.checkApp(r, body.Name, body.Node)
		if err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		writeJSON(w, http.StatusOK, chk)
	})
	api.HandleFunc("POST /api/cluster/apps", func(w http.ResponseWriter, r *http.Request) {
		if refuse(w) {
			return
		}
		var body appBody
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		out, err := c.makeHA(r, cluster.HAApp{Name: body.Name, Data: body.Data, Interval: body.Interval, VIPOnly: body.VIPOnly}, body.Node, true)
		result(w, r, fmt.Sprintf("make %s a high-availability app (%s)", body.Name, body.Data), out, err)
	})
	api.HandleFunc("POST /api/cluster/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		if refuse(w) {
			return
		}
		var body appBody
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		name := r.PathValue("name")
		old, ok := cluster.Load().App(name)
		if !ok {
			writeErr(w, http.StatusNotFound, errors.New("no such HA app"))
			return
		}
		old.Data, old.Interval, old.VIPOnly = body.Data, body.Interval, body.VIPOnly
		out, err := c.makeHA(r, old, "", false)
		result(w, r, "change HA app "+name, out, err)
	})
	api.HandleFunc("POST /api/cluster/apps/{name}/sync", func(w http.ResponseWriter, r *http.Request) {
		if refuse(w) {
			return
		}
		name := r.PathValue("name")
		a, ok := cluster.Load().App(name)
		if !ok {
			writeErr(w, http.StatusNotFound, errors.New("no such HA app"))
			return
		}
		out, err := c.syncApp(a)
		result(w, r, "copy the compose files of HA app "+name+" to every member", out, err)
	})
	api.HandleFunc("POST /api/cluster/apps/{name}/replicate", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		a, ok := cluster.Load().App(name)
		if !ok {
			writeErr(w, http.StatusNotFound, errors.New("no such HA app"))
			return
		}
		_, holder := c.members(cluster.Load())
		if holder == "" {
			writeErr(w, http.StatusConflict, errors.New("no member holds the virtual IP"))
			return
		}
		var out struct{ Pushes []cluster.Push }
		err := c.nodes.JSON(a.Owner, cluster.Self(c.self(), holder), "POST", "/cluster/apps/"+url.PathEscape(name)+"/push", map[string]any{}, &out, 2*time.Hour)
		if err == nil {
			var failed []string
			for _, p := range out.Pushes {
				if !p.OK {
					failed = append(failed, p.Name+": "+p.Error)
				}
			}
			err = errList(failed)
		}
		result(w, r, "copy HA app "+name+" to the other members now", "", err)
	})
	api.HandleFunc("DELETE /api/cluster/apps/{name}", func(w http.ResponseWriter, r *http.Request) {
		if refuse(w) {
			return
		}
		name := r.PathValue("name")
		var app cluster.HAApp
		_, err := cluster.Update(func(s *cluster.State) error {
			i := slices.IndexFunc(s.Apps, func(a cluster.HAApp) bool { return a.Name == name })
			if i < 0 {
				return errors.New("no such HA app")
			}
			app = s.Apps[i]
			s.Apps = slices.Delete(s.Apps, i, i+1)
			return nil
		})
		if err != nil {
			result(w, r, "make "+name+" an ordinary app again", "", err)
			return
		}
		c.sync.push()
		var errs []string
		for _, id := range cluster.Load().MemberIDs() {
			if err := c.nodes.JSON(app.Owner, id, "POST", "/docker/stacks/"+url.PathEscape(name)+"/ha-remove", map[string]any{}, nil, 30*time.Second); err != nil {
				errs = append(errs, c.name(id)+": "+err.Error())
			}
		}
		result(w, r, "make "+name+" an ordinary app again", "It keeps running where it is. The copies on the other members stay, stopped.", errList(errs))
	})
	api.HandleFunc("POST /api/cluster/switchover", func(w http.ResponseWriter, r *http.Request) {
		if refuse(w) {
			return
		}
		var body struct{ To string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		out, err := c.switchover(body.To)
		result(w, r, "move the virtual IP to "+c.name(body.To), out, err)
	})
	return c
}

func (c *clusterMain) self() string {
	if c.readOnly {
		return vip.Load().Self
	}
	return ""
}

// setVIP works out each member's half and applies it: the other members first, then this
// node. Nodes that aren't members anymore turn theirs off.
func (c *clusterMain) setVIP(r *http.Request, enabled bool, address string, prefix int, ids []string, failback bool) (string, error) {
	old := cluster.Load().VIP
	off := func(id string) error {
		if id == cluster.LocalID {
			return vip.Apply(vip.Config{})
		}
		return c.nodes.JSON(userOf(r), id, "POST", "/settings/vip", vip.Config{}, nil, 30*time.Second)
	}
	if !enabled {
		var errs []string
		for _, id := range slices.Backward(old.MemberIDs()) {
			if err := off(id); err != nil {
				errs = append(errs, c.name(id)+": "+err.Error())
			}
		}
		if _, err := cluster.Update(func(s *cluster.State) error { s.VIP.Enabled = false; return nil }); err != nil {
			return "", err
		}
		return "", errList(errs)
	}
	if len(ids) < 2 {
		return "", errors.New("pick at least two member nodes")
	}
	for i, id := range ids {
		if slices.Contains(ids[:i], id) {
			return "", errors.New("each node can be a member once")
		}
	}
	if ip := net.ParseIP(address).To4(); ip == nil {
		return "", errors.New("the virtual IP must be an IPv4 address, like 192.168.1.50")
	}
	draft := cluster.VIPConfig{Enabled: true, Address: address, Prefix: prefix, Failback: failback, RouterID: old.RouterID, Password: old.Password}
	if draft.Password == "" || draft.RouterID == 0 {
		draft.Password, draft.RouterID = vip.NewSecret()
	}
	for _, id := range ids {
		draft.Members = append(draft.Members, cluster.Member{NodeID: id})
	}
	draft.Members = cluster.Priorities(draft.Members)
	ips, ports := map[string]string{}, map[string]int{}
	for _, id := range ids {
		ip, port, err := c.nodeAddr(id, draft)
		if err != nil {
			return "", err
		}
		if ip == address {
			return "", fmt.Errorf("%s is %s's own address; pick a free one", address, c.name(id))
		}
		ips[id], ports[id] = ip, port
	}
	cfgs := map[string]vip.Config{}
	for _, m := range draft.Members {
		var peers []string
		for _, id := range ids {
			if id != m.NodeID {
				peers = append(peers, ips[id])
			}
		}
		role := vip.RoleStandby
		if m.NodeID == cluster.LocalID {
			role = vip.RoleMain
		}
		cfg := vip.Config{Enabled: true, Address: address, Prefix: prefix, Role: role, Priority: m.Priority, Peers: peers,
			Nopreempt: !failback, RouterID: draft.RouterID, Password: draft.Password, Port: ports[m.NodeID], Self: m.NodeID}
		if err := vip.Validate(cfg); err != nil {
			return "", fmt.Errorf("%s: %w", c.name(m.NodeID), err)
		}
		cfgs[m.NodeID] = cfg
	}
	if _, err := cluster.Update(func(s *cluster.State) error { s.VIP = draft; return nil }); err != nil {
		return "", err
	}
	// The members first (they need the main node's state before they can stand in), this
	// node last; then nodes that left let go.
	var errs []string
	for _, id := range ids {
		if id == cluster.LocalID {
			continue
		}
		if err := c.nodes.JSON(userOf(r), id, "POST", "/settings/vip", cfgs[id], nil, 60*time.Second); err != nil {
			errs = append(errs, c.name(id)+": "+err.Error())
		}
	}
	if cfg, ok := cfgs[cluster.LocalID]; ok {
		if err := vip.Apply(cfg); err != nil {
			errs = append(errs, c.name(cluster.LocalID)+": "+err.Error())
		}
	}
	for _, id := range old.MemberIDs() {
		if !slices.Contains(ids, id) {
			if err := off(id); err != nil {
				log.Printf("virtual IP: turning it off on %s: %v", c.name(id), err)
			}
		}
	}
	if !slices.Contains(ids, cluster.LocalID) && vip.Load().Enabled {
		vip.Apply(vip.Config{})
	}
	// The main node's certificate must name the virtual IP: members show it there.
	_, changed, err := tlsutil.EnsureIPs(filepath.Join(config.StateDir, "tls"), []string{address})
	if err != nil {
		errs = append(errs, "certificate: "+err.Error())
	}
	for id, err := range c.sync.push() {
		if err != nil {
			log.Printf("virtual IP: copying to %s: %v", c.name(id), err)
		}
	}
	out := ""
	if changed && !c.local.Dev {
		out = "restart"
		restartSelf()
	}
	return out, errList(errs)
}

// saveShare checks a cluster share, tests it on one node it goes on, and keeps it.
func (c *clusterMain) saveShare(r *http.Request, m storage.Mount, targets []string, password string, create bool) error {
	m.Cluster = true
	clean, err := storage.Normalize(m)
	if err != nil {
		return err
	}
	all := c.nodes.List()
	ids := make([]string, len(all))
	for i, n := range all {
		ids[i] = n.ID
	}
	for _, id := range targets {
		if !slices.Contains(ids, id) {
			return fmt.Errorf("node %s isn't paired", id)
		}
	}
	old, exists := cluster.Load().Share(clean.Name)
	if create && exists {
		return fmt.Errorf("there's already a cluster share named %s", clean.Name)
	}
	if !create && !exists {
		return errors.New("no such cluster share")
	}
	pw := password
	if pw == "" && exists {
		pw = old.Password
	}
	if clean.Type == "smb" && !clean.Guest && pw == "" {
		return errors.New("enter the password")
	}
	sh := cluster.Share{Mount: clean, Nodes: targets, Password: pw}
	if sh.Type != "smb" || sh.Guest {
		sh.Password = ""
	}
	if sh.Nodes == nil {
		sh.Nodes = []string{}
	}
	if exists {
		sh.PassGen = old.PassGen
		if password != "" && password != old.Password {
			sh.PassGen++
		}
	}
	// Test it on the first node it goes on before anything is kept.
	if t := sh.Targets(ids); len(t) > 0 {
		var out struct{ Output string }
		if err := c.nodes.JSON(userOf(r), t[0], "POST", "/storage/test-mount", mountBody{Mount: clean, Password: pw}, &out, time.Minute); err != nil {
			return fmt.Errorf("testing it on %s: %w", c.name(t[0]), err)
		}
	}
	_, err = cluster.Update(func(s *cluster.State) error {
		i := slices.IndexFunc(s.Shares, func(x cluster.Share) bool { return x.Name == sh.Name })
		if i < 0 {
			s.Shares = append(s.Shares, sh)
		} else {
			s.Shares[i] = sh
		}
		return nil
	})
	return err
}

// appCheck is what making an app highly available would take.
type appCheck struct {
	Holder     string            `json:"holder"`
	HolderName string            `json:"holderName"`
	Members    []string          `json:"members"`
	Check      cluster.DataCheck `json:"check"`
	Ports      []string          `json:"ports"`
	config     docker.ComposeFile
}

func (c *clusterMain) checkApp(r *http.Request, name, node string) (appCheck, error) {
	st := cluster.Load()
	var ch appCheck
	if !st.VIP.Enabled {
		return ch, errors.New("set up the virtual IP first (Cluster → Virtual IP)")
	}
	_, holder := c.members(st)
	if holder == "" {
		return ch, errors.New("no member holds the virtual IP right now")
	}
	if node != "" && node != holder {
		return ch, fmt.Errorf("%s holds the virtual IP; HA apps start from the stack there. Move it there first, or move the virtual IP", c.name(holder))
	}
	ch.Holder, ch.HolderName = holder, c.name(holder)
	for _, id := range st.MemberIDs() {
		ch.Members = append(ch.Members, c.name(id))
	}
	user := userOf(r)
	if a, ok := st.App(name); ok {
		user = a.Owner
	}
	var cfg struct {
		Config docker.ComposeFile
		Dir    string
	}
	if err := c.nodes.JSON(user, cluster.Self(c.self(), holder), "GET", "/docker/stacks/"+url.PathEscape(name)+"/config", nil, &cfg, time.Minute); err != nil {
		return ch, fmt.Errorf("%s: %w", ch.HolderName, err)
	}
	ch.config = cfg.Config
	ch.Check = cluster.CheckData(cfg.Config, st.Shares, st.MemberIDs(), storage.MountRoot(), cfg.Dir)
	ch.Ports = []string{}
	for _, svc := range cfg.Config.Services {
		for _, p := range svc.Ports {
			if p.Published != "" && wildcardIP(p.HostIP) {
				ch.Ports = append(ch.Ports, p.Published+"/"+cmpOr(p.Protocol, "tcp"))
			}
		}
	}
	slices.Sort(ch.Ports)
	return ch, nil
}

func wildcardIP(ip string) bool { return ip == "" || ip == "0.0.0.0" || ip == "::" }

func cmpOr(a, b string) string {
	if a != "" {
		return a
	}
	return b
}

// makeHA makes a stack (or changes) an HA app: checks where its data is, keeps the
// settings, sends the members their copy, and recreates it on the holder with the override.
func (c *clusterMain) makeHA(r *http.Request, a cluster.HAApp, node string, create bool) (string, error) {
	c.appMu.Lock()
	defer c.appMu.Unlock()
	if err := cluster.ValidateApp(&a); err != nil {
		return "", err
	}
	if _, exists := cluster.Load().App(a.Name); exists == create {
		if create {
			return "", fmt.Errorf("%s is a high-availability app already", a.Name)
		}
		return "", errors.New("no such HA app")
	}
	ch, err := c.checkApp(r, a.Name, node)
	if err != nil {
		return "", err
	}
	if a.Data == cluster.DataShared && len(ch.Check.Problems) > 0 {
		return "", errors.New("its data isn't all on cluster shares on every member:\n" + strings.Join(ch.Check.Problems, "\n"))
	}
	a.Shares = ch.Check.Shares
	if create {
		a.Owner, a.Created = userOf(r), time.Now().Unix()
	}
	st, err := cluster.Update(func(s *cluster.State) error {
		i := slices.IndexFunc(s.Apps, func(x cluster.HAApp) bool { return x.Name == a.Name })
		if i < 0 {
			s.Apps = append(s.Apps, a)
		} else {
			s.Apps[i] = a
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	// The members learn about it before its files arrive, so none of them starts it.
	c.sync.push()
	out, err := c.writeFiles(st, a, ch.Holder, ch.config)
	if err != nil {
		return out, err
	}
	// The holder runs it with the override from now on.
	holder := cluster.Self(c.self(), ch.Holder)
	var stacks struct{ Stacks []docker.Stack }
	if err := c.nodes.JSON(a.Owner, holder, "GET", "/docker/stacks", nil, &stacks, 30*time.Second); err == nil {
		for _, s := range stacks.Stacks {
			if s.Name == a.Name && s.Running > 0 {
				if err := c.nodes.JSON(a.Owner, holder, "POST", "/docker/stacks/"+url.PathEscape(a.Name)+"/apply", map[string]any{}, nil, 15*time.Minute); err != nil {
					return out, fmt.Errorf("recreating it on %s: %w", ch.HolderName, err)
				}
			}
		}
	}
	if a.Data == cluster.DataReplicated {
		go func() {
			if err := c.nodes.JSON(a.Owner, holder, "POST", "/cluster/apps/"+url.PathEscape(a.Name)+"/push", map[string]any{}, nil, 2*time.Hour); err != nil {
				log.Printf("cluster: first copy of %s: %v", a.Name, err)
			}
		}()
		out = strings.TrimSpace(out + "\nThe first copy to the other members has started.")
	}
	return out, nil
}

// writeFiles sends an HA app's compose files, override and marker to every member.
func (c *clusterMain) writeFiles(st cluster.State, a cluster.HAApp, holder string, cf docker.ComposeFile) (string, error) {
	var files docker.StackFiles
	if err := c.nodes.JSON(a.Owner, cluster.Self(c.self(), holder), "GET", "/docker/stacks/"+url.PathEscape(a.Name), nil, &files, 30*time.Second); err != nil {
		return "", fmt.Errorf("reading it on %s: %w", c.name(holder), err)
	}
	hf := docker.HAFiles{Compose: files.Compose, Env: files.Env, Override: cluster.Override(cf, st.VIP.Address, a.VIPOnly),
		Marker: docker.HAMarker{VIP: st.VIP.Address, Data: a.Data, VIPOnly: a.VIPOnly}}
	var errs []string
	for _, id := range st.MemberIDs() {
		if err := c.nodes.JSON(a.Owner, cluster.Self(c.self(), id), "POST", "/docker/stacks/"+url.PathEscape(a.Name)+"/files", hf, nil, time.Minute); err != nil {
			errs = append(errs, c.name(id)+": "+err.Error())
		}
	}
	if len(errs) > 0 {
		return "", errors.New("copying its files: " + strings.Join(errs, "; "))
	}
	return fmt.Sprintf("Its files are on %d member(s).", len(st.MemberIDs())), nil
}

// syncApp copies an HA app's compose files from the holder to the other members again
// (after it was edited there), with a fresh override.
func (c *clusterMain) syncApp(a cluster.HAApp) (string, error) {
	st := cluster.Load()
	_, holder := c.members(st)
	if holder == "" {
		return "", errors.New("no member holds the virtual IP")
	}
	var cfg struct{ Config docker.ComposeFile }
	if err := c.nodes.JSON(a.Owner, cluster.Self(c.self(), holder), "GET", "/docker/stacks/"+url.PathEscape(a.Name)+"/config", nil, &cfg, time.Minute); err != nil {
		return "", fmt.Errorf("%s: %w", c.name(holder), err)
	}
	return c.writeFiles(st, a, holder, cfg.Config)
}

// appLoop keeps the members' copies of HA apps' compose files in step with the holder's.
func (c *clusterMain) appLoop() {
	for {
		time.Sleep(5 * time.Minute)
		for _, a := range cluster.Load().Apps {
			c.appMu.Lock()
			if _, err := c.syncApp(a); err != nil {
				log.Printf("cluster: keeping %s's files in step: %v", a.Name, err)
			}
			c.appMu.Unlock()
		}
	}
}

// switchover moves the virtual IP to one member on purpose: the holder stops the HA apps
// and sends the final copies, every other member gives the address up so only the target
// can take it, and then they all compete for it again (with no automatic failback, it
// stays on the target).
func (c *clusterMain) switchover(to string) (string, error) {
	if !c.switchMu.TryLock() {
		return "", errors.New("a switchover is running already")
	}
	defer c.switchMu.Unlock()
	st := cluster.Load()
	if !st.VIP.Enabled || !st.IsMember(to) {
		return "", errors.New("pick a member of the virtual IP")
	}
	members, holder := c.members(st)
	for _, m := range members {
		if m.NodeID == to && (!m.Online || m.VIP == nil || !m.VIP.Active) {
			return "", fmt.Errorf("%s isn't ready to take it (offline, or keepalived isn't running there)", m.Name)
		}
	}
	if holder == to {
		return c.name(to) + " holds it already.", nil
	}
	var notes []string
	if holder != "" {
		var out struct{ Notes []string }
		if err := c.nodes.JSON("", holder, "POST", "/cluster/handoff", map[string]string{"to": to}, &out, 2*time.Hour); err != nil {
			return "", fmt.Errorf("stopping the HA apps on %s: %w", c.name(holder), err)
		}
		notes = out.Notes
	}
	release := func(on bool) {
		for _, m := range members {
			if m.NodeID == to && on {
				continue
			}
			if err := c.nodes.JSON("", m.NodeID, "POST", "/cluster/release", map[string]bool{"on": on}, nil, 15*time.Second); err != nil && m.Online {
				log.Printf("cluster: release %v on %s: %v", on, m.Name, err)
			}
		}
		if holder != "" && !on {
			c.nodes.JSON("", holder, "POST", "/cluster/handoff/done", map[string]any{}, nil, 15*time.Second)
		}
	}
	release(true)
	defer release(false)
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		var vs vip.Status
		if err := c.nodes.JSON("", to, "GET", "/settings/vip", nil, &vs, 5*time.Second); err == nil && vs.Holds {
			out := c.name(to) + " holds the virtual IP now."
			if len(notes) > 0 {
				out += "\n" + strings.Join(notes, "\n")
			}
			return out, nil
		}
		time.Sleep(2 * time.Second)
	}
	return strings.Join(notes, "\n"), fmt.Errorf("%s didn't take the virtual IP within a minute; it stays where it was", c.name(to))
}

// exportClusterShares is the cluster shares, with their passwords, for an export.
func exportClusterShares() (json.RawMessage, []string) {
	st := cluster.Load()
	if len(st.Shares) == 0 {
		return nil, nil
	}
	b, _ := json.Marshal(st.Shares)
	names := make([]string, len(st.Shares))
	for i, sh := range st.Shares {
		names[i] = sh.Name
	}
	return b, names
}

// importShares puts the cluster shares from an export back (replacing ones with the same
// name) and mounts them on their nodes.
func (c *clusterMain) importShares(raw json.RawMessage) (string, error) {
	var list []cluster.Share
	if err := json.Unmarshal(raw, &list); err != nil {
		return "", fmt.Errorf("reading them: %w", err)
	}
	var names []string
	_, err := cluster.Update(func(s *cluster.State) error {
		for _, sh := range list {
			if sh.Nodes == nil {
				sh.Nodes = []string{}
			}
			sh.Cluster = true
			names = append(names, sh.Name)
			if i := slices.IndexFunc(s.Shares, func(x cluster.Share) bool { return x.Name == sh.Name }); i >= 0 {
				sh.PassGen = s.Shares[i].PassGen + 1
				s.Shares[i] = sh
			} else {
				s.Shares = append(s.Shares, sh)
			}
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	c.Kick()
	return fmt.Sprintf("cluster shares %s restored; they're mounted on their nodes in the background (Cluster → Shared storage)", strings.Join(names, ", ")), nil
}
