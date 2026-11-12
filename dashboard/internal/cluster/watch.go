package cluster

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/activity"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/backup"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/docker"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/nodes"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/storage"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/tlsutil"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/vip"
)

// The watcher runs on every node. It looks every 2 seconds whether this node holds the
// virtual IP, and on every member it starts the HA apps where the address is and stops
// them everywhere else. On the holder it also keeps a shared app's lock fresh and sends a
// replicated app's copy to the other members on its timer.

const (
	pollEvery      = 2 * time.Second
	reconcileEvery = lockBeat
	retryAfter     = 30 * time.Second
)

// AppLocal is what a node remembers about an HA app across restarts (ha-local.json).
type AppLocal struct {
	ActiveFrom  int64  `json:"activeFrom,omitempty"` // when it last started here
	ActiveTo    int64  `json:"activeTo,omitempty"`   // the last time it was seen running here
	ReplicaAt   int64  `json:"replicaAt,omitempty"`  // when the copy here was taken (the sender's clock)
	ReplicaFrom string `json:"replicaFrom,omitempty"`
	ReplicaSize int64  `json:"replicaSize,omitempty"`
}

// Push is the last copy of a replicated app sent to one member.
type Push struct {
	Node  string `json:"node"`
	Name  string `json:"name"`
	At    int64  `json:"at"`
	OK    bool   `json:"ok"`
	Size  int64  `json:"size,omitempty"`
	Error string `json:"error,omitempty"`
}

// AppStatus is one HA app on one node, for the Cluster page.
type AppStatus struct {
	Name    string `json:"name"`
	Present bool   `json:"present"` // the stack's folder, with the HA marker, is on this node
	Running bool   `json:"running"`
	State   string `json:"state"` // running, starting, stopping, waiting, failed, standby, missing
	Detail  string `json:"detail,omitempty"`
	Lock    string `json:"lock,omitempty"`
	AppLocal
	Pushes []Push `json:"pushes,omitempty"`
}

// LocalStatus is this node's side of the cluster.
type LocalStatus struct {
	Host      string      `json:"host"`
	Enabled   bool        `json:"enabled"` // a member of the virtual IP
	Holds     bool        `json:"holds"`
	Releasing bool        `json:"releasing"`
	Rev       int64       `json:"rev"` // the cluster settings this node has
	Apps      []AppStatus `json:"apps"`
}

type appRun struct {
	state, detail, lock string
	busy                bool
	retryAt             time.Time
	nextPush            time.Time
	pushing             bool
	pushes              map[string]Push
}

type Watcher struct {
	dk   *docker.Client
	bk   *backup.Manager
	host string

	mu         sync.Mutex
	known      bool
	holds      bool
	lastRun    time.Time
	apps       map[string]*appRun
	local      map[string]AppLocal
	handingOff time.Time // until then, act as if the address were gone (a switchover)

	storeAt time.Time
	store   *nodes.Store
}

// OwnerOverride replaces each app's owner (development: the dev server runs as one user).
var OwnerOverride string

var (
	startOnce sync.Once
	current   *Watcher
)

// Start runs the watcher once per process.
func Start(dk *docker.Client, bk *backup.Manager) *Watcher {
	startOnce.Do(func() {
		h, _ := os.Hostname()
		current = &Watcher{dk: dk, bk: bk, host: h, apps: map[string]*appRun{}, local: loadLocal()}
		go current.run()
	})
	return current
}

// Current is the running watcher, or nil.
func Current() *Watcher { return current }

func localPath() string { return filepath.Join(config.StateDir, "ha-local.json") }

func loadLocal() map[string]AppLocal {
	m := map[string]AppLocal{}
	if b, err := os.ReadFile(localPath()); err == nil {
		json.Unmarshal(b, &m)
	}
	return m
}

// saveLocal writes ha-local.json; the caller holds w.mu.
func (w *Watcher) saveLocalLocked() {
	b, _ := json.MarshalIndent(w.local, "", "  ")
	tmp := localPath() + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err == nil {
		os.Rename(tmp, localPath())
	}
}

func (w *Watcher) setLocal(name string, fn func(a *AppLocal)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	a := w.local[name]
	fn(&a)
	w.local[name] = a
	w.saveLocalLocked()
}

func (w *Watcher) runOf(name string) *appRun {
	r := w.apps[name]
	if r == nil {
		r = &appRun{state: "standby", pushes: map[string]Push{}}
		w.apps[name] = r
	}
	return r
}

func (w *Watcher) set(name string, fn func(r *appRun)) {
	w.mu.Lock()
	defer w.mu.Unlock()
	fn(w.runOf(name))
}

func (w *Watcher) owner(a HAApp) (docker.Owner, error) {
	name := a.Owner
	if OwnerOverride != "" {
		name = OwnerOverride
	}
	return docker.LookupOwner(name)
}

func record(action string, err error) {
	if err != nil {
		log.Printf("cluster: %s failed: %v", action, err)
	} else {
		log.Printf("cluster: %s", action)
	}
	e := activity.Entry{Via: "cluster", Action: action, OK: err == nil}
	if err != nil {
		e.Error = err.Error()
	}
	activity.Add(e)
}

func (w *Watcher) run() {
	t := time.NewTicker(pollEvery)
	for {
		w.tick()
		<-t.C
	}
}

// effectiveHolds is whether HA apps belong here: the address is here and isn't being
// handed to another member.
func (w *Watcher) effectiveHolds() bool {
	w.mu.Lock()
	handing := time.Now().Before(w.handingOff)
	w.mu.Unlock()
	return vip.Holds() && !vip.Releasing() && !handing
}

func (w *Watcher) tick() {
	if !vip.Load().Enabled {
		w.mu.Lock()
		w.known = false
		w.mu.Unlock()
		return // not a member: leave every app alone
	}
	holds := w.effectiveHolds()
	w.mu.Lock()
	changed := !w.known || holds != w.holds
	due := time.Since(w.lastRun) >= reconcileEvery
	if changed && w.known {
		if holds {
			log.Printf("cluster: this node holds the virtual IP now")
		} else {
			log.Printf("cluster: this node doesn't hold the virtual IP anymore")
		}
	}
	w.known, w.holds = true, holds
	retry := false
	for _, r := range w.apps {
		retry = retry || (!r.busy && (r.state == "waiting" || r.state == "failed") && time.Now().After(r.retryAt))
	}
	w.mu.Unlock()
	if changed || due || retry {
		w.reconcile(holds)
	}
}

func running(cs []docker.Container, project string) bool {
	for _, c := range cs {
		if c.Project() == project && (c.State == "running" || c.State == "paused" || c.State == "restarting") {
			return true
		}
	}
	return false
}

// reconcile makes every HA app here match where the address is.
func (w *Watcher) reconcile(holds bool) {
	st := Load()
	w.mu.Lock()
	w.lastRun = time.Now()
	for name := range w.apps {
		if _, ok := st.App(name); !ok {
			delete(w.apps, name)
		}
	}
	w.mu.Unlock()
	if len(st.Apps) == 0 {
		return
	}
	cs, err := w.dk.Containers()
	if err != nil {
		for _, a := range st.Apps {
			w.set(a.Name, func(r *appRun) { r.state, r.detail = "failed", "Docker isn't reachable: "+err.Error() })
		}
		return
	}
	now := time.Now()
	for _, a := range st.Apps {
		o, err := w.owner(a)
		if err != nil {
			w.set(a.Name, func(r *appRun) { r.state, r.detail = "missing", err.Error() })
			continue
		}
		dir, err := docker.StackDir(o, a.Name)
		if err != nil || docker.ReadHA(dir) == nil {
			w.set(a.Name, func(r *appRun) { r.state, r.detail = "missing", "this node has no copy of it" })
			continue
		}
		up := running(cs, a.Name)
		w.mu.Lock()
		r := w.runOf(a.Name)
		if r.busy {
			w.mu.Unlock()
			continue
		}
		switch {
		case holds && up:
			r.state, r.detail = "running", ""
			push := a.Copies() && !r.pushing && now.After(r.nextPush)
			if push {
				r.pushing = true
				r.nextPush = now.Add(time.Duration(max(a.Interval, 10)) * time.Second)
			}
			w.mu.Unlock()
			w.heartbeat(a, now)
			if push {
				go w.push(a, o, nil)
			}
		case holds && !up:
			if now.Before(r.retryAt) {
				w.mu.Unlock()
				continue
			}
			r.busy, r.state, r.detail = true, "starting", ""
			w.mu.Unlock()
			go w.start(st, a, o)
		case !holds && up:
			r.busy, r.state, r.detail = true, "stopping", ""
			w.mu.Unlock()
			go w.stop(a, o)
		default:
			r.state, r.detail = "standby", "" // it's down, as it should be here
			r.lock = ""
			w.mu.Unlock()
		}
	}
}

func (w *Watcher) lockFile(a HAApp) string {
	if a.Data != DataShared || len(a.Shares) == 0 {
		return ""
	}
	return lockPath(storage.MountRoot(), a.Shares[0], a.Name)
}

// heartbeat says the app still runs here: its lock and its last-seen time.
func (w *Watcher) heartbeat(a HAApp, now time.Time) {
	if p := w.lockFile(a); p != "" {
		note := "held by this node"
		if err := writeLock(p, w.host, now); err != nil {
			note = "couldn't write the lock: " + err.Error()
		}
		w.set(a.Name, func(r *appRun) { r.lock = note })
	}
	w.setLocal(a.Name, func(l *AppLocal) {
		l.ActiveTo = now.Unix()
		if l.ActiveFrom == 0 {
			l.ActiveFrom = now.Unix()
		}
	})
}

var errPeerRunning = errors.New("still running on another member")

func (w *Watcher) start(st State, a HAApp, o docker.Owner) {
	defer w.set(a.Name, func(r *appRun) { r.busy = false })
	wait := func(detail string) {
		w.set(a.Name, func(r *appRun) {
			r.state, r.detail, r.retryAt = "waiting", detail, time.Now().Add(5*time.Second)
		})
	}
	note := ""
	if p := w.lockFile(a); p != "" {
		l, err := readLock(p)
		if err == nil {
			if who := lockBlocker(l, w.host, time.Now()); who != "" {
				wait(fmt.Sprintf("waiting for %s to release it (its lock is %ds old)", who, time.Now().Unix()-l.At))
				w.set(a.Name, func(r *appRun) { r.lock = "held by " + who })
				return
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			note = "couldn't read the lock: " + err.Error()
		}
		if err := writeLock(p, w.host, time.Now()); err != nil {
			note = "couldn't write the lock: " + err.Error()
			w.set(a.Name, func(r *appRun) { r.lock = note })
		} else {
			w.set(a.Name, func(r *appRun) { r.lock = "held by this node" })
		}
	}
	if a.Copies() {
		n, err := w.catchUp(st, a, o)
		if errors.Is(err, errPeerRunning) {
			wait(n)
			return
		}
		note = n
		if a.Data == DataSynced && backup.SyncInterrupted(config.StateDir, a.Name) {
			note = strings.TrimPrefix(note+"; the last sync here broke off halfway, so some files may be from a moment earlier than others", "; ")
		}
	}
	out, err := w.dk.StackAction(o, a.Name, "start")
	if err != nil {
		err = fmt.Errorf("%v %s", err, out)
		w.set(a.Name, func(r *appRun) {
			r.state, r.detail, r.retryAt = "failed", err.Error(), time.Now().Add(retryAfter)
		})
		record("start HA app "+a.Name+" (this node holds the virtual IP)", err)
		return
	}
	now := time.Now().Unix()
	w.setLocal(a.Name, func(l *AppLocal) { l.ActiveFrom, l.ActiveTo = now, now })
	w.set(a.Name, func(r *appRun) {
		r.state, r.detail = "running", note
		r.nextPush = time.Now().Add(time.Minute) // first copy soon after it's up
	})
	what := "start HA app " + a.Name + " (this node holds the virtual IP)"
	if note != "" {
		what += ": " + note
	}
	record(what, nil)
}

func (w *Watcher) stop(a HAApp, o docker.Owner) {
	defer w.set(a.Name, func(r *appRun) { r.busy = false })
	out, err := w.dk.StackAction(o, a.Name, "stop")
	if err != nil {
		err = fmt.Errorf("%v %s", err, out)
		w.set(a.Name, func(r *appRun) { r.state, r.detail = "failed", err.Error() })
		record("stop HA app "+a.Name+" (the virtual IP moved away)", err)
		return
	}
	w.setLocal(a.Name, func(l *AppLocal) { l.ActiveTo = time.Now().Unix() })
	if p := w.lockFile(a); p != "" {
		removeLock(p, w.host)
	}
	w.set(a.Name, func(r *appRun) { r.state, r.detail, r.lock = "standby", "", "" })
	record("stop HA app "+a.Name+" (the virtual IP moved away)", nil)
}

// ---- Other members --------------------------------------------------------------------

// peer is another member this node can call.
type peer struct {
	ID   string // in the main node's list
	Name string
	node nodes.Node
}

// nodeStore is nodes.json (the main node's own list, or the copy a member got), read again
// every minute.
func (w *Watcher) nodeStore() *nodes.Store {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.store == nil || time.Since(w.storeAt) > time.Minute {
		if s, err := nodes.Open(); err == nil {
			w.store, w.storeAt = s, time.Now()
		}
	}
	return w.store
}

// peers are the other members of the virtual IP.
func (w *Watcher) peers(st State) ([]peer, *nodes.Store) {
	store := w.nodeStore()
	if store == nil {
		return nil, nil
	}
	v := vip.Load()
	self := v.Self
	if self == "" {
		self = LocalID
		if v.Role != vip.RoleMain {
			self = "?"
		}
	}
	var out []peer
	for _, id := range st.MemberIDs() {
		if id == self {
			continue
		}
		if id == LocalID {
			// The main node, through its cluster API.
			if v.MainAddr == "" || st.Secret == "" {
				continue
			}
			c, err := tls.LoadX509KeyPair(filepath.Join(vip.StandbyTLSDir(), "cert.pem"), filepath.Join(vip.StandbyTLSDir(), "key.pem"))
			if err != nil {
				continue
			}
			out = append(out, peer{ID: LocalID, Name: v.MainName, node: nodes.Node{ID: "cluster-main", Name: v.MainName,
				Address: v.MainAddr, Token: st.Secret, Fingerprint: tlsutil.Fingerprint(c)}})
			continue
		}
		if n, ok := store.Get(id); ok {
			out = append(out, peer{ID: id, Name: n.Name, node: n})
		}
	}
	return out, store
}

func (w *Watcher) peerStatus(store *nodes.Store, p peer) (LocalStatus, error) {
	var ls LocalStatus
	err := store.GetJSON(p.node, "/cluster/local", 5*time.Second, &ls)
	return ls, err
}

// catchUp makes sure a replicated app starts from the newest copy: when another member ran
// it after the copy here was taken, the final copy comes from there first. It returns a
// note for the Cluster page.
func (w *Watcher) catchUp(st State, a HAApp, o docker.Owner) (string, error) {
	w.mu.Lock()
	mine := w.local[a.Name]
	w.mu.Unlock()
	have := max(mine.ReplicaAt, mine.ActiveTo)
	peers, store := w.peers(st)
	var best *peer
	var bestTo int64
	reached := 0
	var silent []string
	for i, p := range peers {
		ls, err := w.peerStatus(store, p)
		if err != nil {
			silent = append(silent, p.Name)
			continue
		}
		reached++
		for _, as := range ls.Apps {
			if as.Name != a.Name {
				continue
			}
			if as.Running {
				return fmt.Sprintf("waiting for %s to stop it", p.Name), errPeerRunning
			}
			if as.ActiveTo > have && as.ActiveTo > bestTo {
				best, bestTo = &peers[i], as.ActiveTo
			}
		}
	}
	if best != nil && a.Data == DataSynced {
		// The peer sends its changes here, as it does when it runs the app.
		w.set(a.Name, func(r *appRun) { r.detail = "getting the latest changes from " + best.Name })
		var out struct{ Pushes []Push }
		err := store.PostBody(best.node, a.Owner, "/cluster/apps/"+url.PathEscape(a.Name)+"/push",
			2*time.Hour, map[string]any{"only": []string{w.selfID()}, "handover": true}, &out)
		if err == nil && (len(out.Pushes) == 0 || !out.Pushes[0].OK) {
			err = errors.New("it couldn't reach this node")
			if len(out.Pushes) > 0 {
				err = errors.New(out.Pushes[0].Error)
			}
		}
		record(fmt.Sprintf("get the latest changes of HA app %s from %s", a.Name, best.Name), err)
		if err != nil {
			return fmt.Sprintf("started from the copy synced %s; getting the latest from %s failed: %v", ago(mine.ReplicaAt), best.Name, err), nil
		}
		return unanswered(silent), nil
	}
	if best != nil {
		w.set(a.Name, func(r *appRun) { r.detail = "getting the latest copy from " + best.Name })
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		resp, err := store.Request(ctx, best.node, "GET", "/cluster/snapshot/out?stack="+url.QueryEscape(a.Name), a.Owner, "cluster", nil, "")
		if err == nil && resp.StatusCode != http.StatusOK {
			err = errorOf(resp)
			resp.Body.Close()
		}
		if err == nil {
			cr := &countReader{r: resp.Body}
			var man backup.SnapshotManifest
			man, err = w.bk.SnapshotIn(o, cr)
			resp.Body.Close()
			if err == nil {
				w.setLocal(a.Name, func(l *AppLocal) { l.ReplicaAt, l.ReplicaFrom, l.ReplicaSize = man.TakenAt, best.Name, cr.n })
				record(fmt.Sprintf("get the latest copy of HA app %s from %s", a.Name, best.Name), nil)
				return unanswered(silent), nil
			}
		}
		record(fmt.Sprintf("get the latest copy of HA app %s from %s", a.Name, best.Name), err)
		return fmt.Sprintf("started from the copy taken %s; getting the latest from %s failed: %v", ago(mine.ReplicaAt), best.Name, err), nil
	}
	if reached == 0 && len(peers) > 0 && mine.ActiveTo < mine.ReplicaAt {
		note := "started from the copy taken " + ago(mine.ReplicaAt) + " (no other member answered)"
		record(fmt.Sprintf("HA app %s %s", a.Name, note), nil)
		return note, nil
	}
	return unanswered(silent), nil
}

// unanswered notes the members that didn't answer a takeover check: changes made there
// since their last copy didn't come along.
func unanswered(names []string) string {
	if len(names) == 0 {
		return ""
	}
	return strings.Join(names, ", ") + " didn't answer; changes made there after its last copy aren't here"
}

func ago(unix int64) string {
	if unix == 0 {
		return "never (no copy had arrived)"
	}
	d := time.Since(time.Unix(unix, 0)).Round(time.Second)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds ago", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min ago", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%.1f h ago", d.Hours())
	}
	return fmt.Sprintf("%d days ago", int(d.Hours()/24))
}

type countReader struct {
	r io.Reader
	n int64
}

func (c *countReader) Read(b []byte) (int, error) {
	n, err := c.r.Read(b)
	c.n += int64(n)
	return n, err
}

func errorOf(resp *http.Response) error {
	var e struct{ Error string }
	json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&e)
	if e.Error == "" {
		e.Error = resp.Status
	}
	return errors.New(e.Error)
}

// push sends a copy of a replicated app to the other members (or only to those in only).
func (w *Watcher) push(a HAApp, o docker.Owner, only []string) []Push {
	defer w.set(a.Name, func(r *appRun) { r.pushing = false })
	peers, store := w.peers(Load())
	var results []Push
	for _, p := range peers {
		if len(only) > 0 && !slices.Contains(only, p.ID) {
			continue
		}
		res := Push{Node: p.ID, Name: p.Name, At: time.Now().Unix()}
		var n int64
		var err error
		if a.Data == DataSynced {
			n, err = w.pushSync(store, p, a, o)
		} else {
			n, err = w.pushSnapshot(store, p, a, o)
		}
		res.Size, res.OK = n, err == nil
		if err != nil {
			res.Error = err.Error()
			log.Printf("cluster: copying HA app %s to %s: %v", a.Name, p.Name, err)
		}
		results = append(results, res)
		w.set(a.Name, func(r *appRun) { r.pushes[p.ID] = res })
	}
	return results
}

// pushSnapshot sends a replicated app's full copy to one member.
func (w *Watcher) pushSnapshot(store *nodes.Store, p peer, a HAApp, o docker.Owner) (int64, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(w.bk.SnapshotOut(ctx, pw, o, a.Name)) }()
	cr := &countReader{r: pr}
	resp, err := store.Request(ctx, p.node, "POST", "/cluster/snapshot/in?stack="+url.QueryEscape(a.Name), a.Owner, "cluster", cr, "application/gzip")
	if err == nil {
		if resp.StatusCode != http.StatusOK {
			err = errorOf(resp)
		}
		resp.Body.Close()
	}
	pr.CloseWithError(errors.New("done"))
	return cr.n, err
}

// syncPauseLimit: changes up to this size are sent with the app paused, so they match the
// list; bigger ones (the first sync, say) are sent running, and the next sync catches up.
const syncPauseLimit = 64 << 20

// pushSync sends a synced app's changes to one member: the file list, then what it lacks.
func (w *Watcher) pushSync(store *nodes.Store, p peer, a HAApp, o docker.Owner) (int64, error) {
	idx, resume, err := w.bk.MakeSyncIndex(o, a.Name, true)
	if err != nil {
		return 0, err
	}
	defer func() { resume() }()
	var need struct{ Need []string }
	if err := store.PostBody(p.node, a.Owner, "/cluster/sync/need?stack="+url.QueryEscape(a.Name), 5*time.Minute, idx, &need); err != nil {
		return 0, err
	}
	if len(need.Need) == 0 {
		// Nothing to send, but the member's copy is now this fresh.
		need.Need = []string{}
	}
	sizes := map[string]int64{}
	for _, e := range idx.Entries {
		sizes[e.Path] = e.Size
	}
	var total int64
	for _, n := range need.Need {
		total += sizes[n]
	}
	if total > syncPauseLimit {
		resume()
		resume = func() {}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
	defer cancel()
	pr, pw := io.Pipe()
	go func() { pw.CloseWithError(w.bk.SyncOut(ctx, pw, o, idx, need.Need)) }()
	cr := &countReader{r: pr}
	resp, err := store.Request(ctx, p.node, "POST", "/cluster/sync/in?stack="+url.QueryEscape(a.Name), a.Owner, "cluster", cr, "application/gzip")
	if err == nil {
		if resp.StatusCode != http.StatusOK {
			err = errorOf(resp)
		}
		resp.Body.Close()
	}
	pr.CloseWithError(errors.New("done"))
	return cr.n, err
}

// selfID is this node in the main node's list.
func (w *Watcher) selfID() string {
	v := vip.Load()
	if v.Self != "" {
		return v.Self
	}
	return LocalID
}

// ---- What the API calls ----------------------------------------------------------------

// Status is this node's side, for GET /cluster/local.
func (w *Watcher) Status() LocalStatus {
	st := Load()
	v := vip.Load()
	ls := LocalStatus{Host: w.host, Enabled: v.Enabled, Holds: vip.Holds(), Releasing: vip.Releasing(), Rev: st.Rev, Apps: []AppStatus{}}
	var cs []docker.Container
	if len(st.Apps) > 0 {
		cs, _ = w.dk.Containers()
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	for _, a := range st.Apps {
		as := AppStatus{Name: a.Name, AppLocal: w.local[a.Name], State: "standby"}
		if o, err := w.owner(a); err == nil {
			if dir, err := docker.StackDir(o, a.Name); err == nil && docker.ReadHA(dir) != nil {
				as.Present = true
			}
		}
		as.Running = running(cs, a.Name)
		if r := w.apps[a.Name]; r != nil {
			as.State, as.Detail, as.Lock = r.state, r.detail, r.lock
			for _, p := range r.pushes {
				as.Pushes = append(as.Pushes, p)
			}
			sort.Slice(as.Pushes, func(i, j int) bool { return as.Pushes[i].Name < as.Pushes[j].Name })
		}
		if !as.Present {
			as.State = "missing"
		}
		ls.Apps = append(ls.Apps, as)
	}
	return ls
}

// Received records a replica that arrived.
func (w *Watcher) Received(name string, man backup.SnapshotManifest, size int64) {
	w.setLocal(name, func(l *AppLocal) { l.ReplicaAt, l.ReplicaFrom, l.ReplicaSize = man.TakenAt, man.Host, size })
}

// PushNow sends an app's copy (replicated) or changes (synced) to the other members now
// (only: to those), and waits for it. With handover, a member that doesn't hold the virtual
// IP sends its copy too, if the app doesn't run there: the member taking over asks for it.
func (w *Watcher) PushNow(name string, only []string, handover bool) ([]Push, error) {
	a, ok := Load().App(name)
	if !ok || !a.Copies() {
		return nil, errors.New("no such replicated or synced HA app")
	}
	o, err := w.owner(a)
	if err != nil {
		return nil, err
	}
	if !vip.Holds() && !handover {
		return nil, errors.New("this node doesn't hold the virtual IP")
	}
	if handover && !vip.Holds() {
		if cs, err := w.dk.Containers(); err != nil || running(cs, name) {
			return nil, errors.New("it still runs here")
		}
	}
	w.mu.Lock()
	r := w.runOf(name)
	if r.pushing {
		w.mu.Unlock()
		return nil, errors.New("a copy is being sent already")
	}
	r.pushing = true
	w.mu.Unlock()
	return w.push(a, o, only), nil
}

// HandOff is the first half of a switchover on the holder: it stops the HA apps and keeps
// them stopped for a few minutes, then sends the replicated ones' final copy to the
// member taking over.
func (w *Watcher) HandOff(to string) ([]string, error) {
	st := Load()
	w.mu.Lock()
	w.handingOff = time.Now().Add(3 * time.Minute)
	w.mu.Unlock()
	var notes []string
	for _, a := range st.Apps {
		o, err := w.owner(a)
		if err != nil {
			continue
		}
		w.set(a.Name, func(r *appRun) { r.busy = true; r.state = "stopping" })
		w.stop(a, o)
		if !a.Copies() {
			continue
		}
		w.set(a.Name, func(r *appRun) { r.pushing = true })
		for _, p := range w.push(a, o, []string{to}) {
			if !p.OK {
				notes = append(notes, fmt.Sprintf("%s: the final copy to %s failed: %s", a.Name, p.Name, p.Error))
			}
		}
	}
	return notes, nil
}

// HandOffDone ends a switchover (or calls one off): the apps may start here again if the
// address is still here.
func (w *Watcher) HandOffDone() {
	w.mu.Lock()
	w.handingOff = time.Time{}
	w.lastRun = time.Time{}
	w.mu.Unlock()
}
