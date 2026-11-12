package cluster

import (
	"fmt"
	"log"
	"net/url"
	"slices"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/storage"
)

// NodeInfo is one node the main node manages; the main node itself is LocalID.
type NodeInfo struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// Nodes is how the main node reaches every node's API: its own in-process, the paired ones
// by token. user is who acts ("" for the main node itself).
type Nodes interface {
	List() []NodeInfo
	JSON(user, id, method, path string, body, out any, timeout time.Duration) error
}

// NodeShare is one cluster share on one node.
type NodeShare struct {
	Share string `json:"-"`
	Node  string `json:"node"`
	Name  string `json:"name"`  // the node's
	State string `json:"state"` // mounted, waiting, mounting, failed, off, offline
	Error string `json:"error,omitempty"`
}

// Shares keeps every cluster share mounted where it should be: on each change, every five
// minutes, and when a node is paired. It goes through each node's own Network shares API,
// so a node tests a share before it writes the units, as when it's added there by hand.
type Shares struct {
	nodes Nodes
	kick  chan struct{}
	run   sync.Mutex

	mu      sync.Mutex
	status  map[string][]NodeShare // by share
	applied map[string]int         // node/share → the password generation it has
	at      int64
}

func NewShares(n Nodes) *Shares {
	return &Shares{nodes: n, kick: make(chan struct{}, 1), status: map[string][]NodeShare{}, applied: map[string]int{}}
}

// Loop reconciles every five minutes and whenever kicked.
func (r *Shares) Loop() {
	t := time.NewTicker(5 * time.Minute)
	for {
		r.Once()
		select {
		case <-t.C:
		case <-r.kick:
		}
	}
}

// Kick reconciles soon.
func (r *Shares) Kick() {
	select {
	case r.kick <- struct{}{}:
	default:
	}
}

// Status is the last result per share, and when it ran.
func (r *Shares) Status() (map[string][]NodeShare, int64) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := map[string][]NodeShare{}
	for k, v := range r.status {
		out[k] = slices.Clone(v)
	}
	return out, r.at
}

// want is the mount a node should have for a share.
func want(sh Share) storage.Mount {
	m := sh.Mount
	m.Cluster = true
	if n, err := storage.Normalize(m); err == nil {
		m = n
	}
	return m
}

// differs reports whether a node's mount isn't what the share says.
func differs(cur, w storage.Mount) bool {
	return cur.Type != w.Type || cur.Server != w.Server || cur.Path != w.Path || cur.Version != w.Version ||
		cur.ReadOnly != w.ReadOnly || cur.Automount != w.Automount || cur.DockerWaits != w.DockerWaits ||
		cur.Username != w.Username || cur.Domain != w.Domain || cur.Guest != w.Guest || cur.Owner != w.Owner ||
		cur.FileMode != w.FileMode || cur.DirMode != w.DirMode
}

type mountBody struct {
	storage.Mount
	Password string `json:"password"`
}

// Once reconciles every node now.
func (r *Shares) Once() {
	r.run.Lock()
	defer r.run.Unlock()
	st := Load()
	all := r.nodes.List()
	ids := make([]string, len(all))
	for i, n := range all {
		ids[i] = n.ID
	}
	results := make([][]NodeShare, len(all))
	var wg sync.WaitGroup
	for i, n := range all {
		wg.Add(1)
		go func() {
			defer wg.Done()
			results[i] = r.node(st, n, ids)
		}()
	}
	wg.Wait()
	status := map[string][]NodeShare{}
	for _, sh := range st.Shares {
		status[sh.Name] = []NodeShare{}
	}
	for _, res := range results {
		for _, ns := range res {
			status[ns.Share] = append(status[ns.Share], ns)
		}
	}
	r.mu.Lock()
	r.status, r.at = status, time.Now().Unix()
	r.mu.Unlock()
}

// node makes one node's cluster shares match; it returns one NodeShare per share meant for it.
func (r *Shares) node(st State, n NodeInfo, ids []string) []NodeShare {
	var mine []Share
	for _, sh := range st.Shares {
		if slices.Contains(sh.Targets(ids), n.ID) {
			mine = append(mine, sh)
		}
	}
	var list struct{ Mounts []storage.Mount }
	if err := r.nodes.JSON("", n.ID, "GET", "/storage/mounts", nil, &list, 20*time.Second); err != nil {
		var out []NodeShare
		for _, sh := range mine {
			out = append(out, NodeShare{Share: sh.Name, Node: n.ID, Name: n.Name, State: "offline", Error: err.Error()})
		}
		return out
	}
	have := map[string]storage.Mount{}
	for _, m := range list.Mounts {
		have[m.Name] = m
	}
	var out []NodeShare
	for _, sh := range mine {
		w := want(sh)
		key := n.ID + "/" + sh.Name
		cur, ok := have[sh.Name]
		res := NodeShare{Share: sh.Name, Node: n.ID, Name: n.Name, State: cur.State}
		r.mu.Lock()
		gen, seen := r.applied[key]
		r.mu.Unlock()
		var err error
		var verb string
		switch {
		case !ok:
			verb = "add"
			err = r.nodes.JSON("", n.ID, "POST", "/storage/mounts", mountBody{w, sh.Password}, nil, 2*time.Minute)
		case !cur.Cluster && cur.Type == w.Type && cur.Server == w.Server && cur.Path == w.Path:
			// The same share, unmarked: a dashboard from before cluster shares mounted it.
			res.State, res.Error = "failed", fmt.Sprintf("%s mounted it but didn't mark it as a cluster share; its dashboard is probably older. Update %s, then remove the share there (Files → Network shares) so the cluster adds it again", n.Name, n.Name)
		case !cur.Cluster:
			res.State, res.Error = "failed", fmt.Sprintf("%s already has a share named %s of its own; rename or remove it there", n.Name, sh.Name)
		case differs(cur, w) || (seen && gen != sh.PassGen) || (w.Type == "smb" && !w.Guest && !cur.HasPassword):
			verb = "change"
			err = r.nodes.JSON("", n.ID, "POST", "/storage/mounts/"+url.PathEscape(sh.Name), mountBody{w, sh.Password}, nil, 2*time.Minute)
		}
		if verb != "" {
			if err != nil {
				res.State, res.Error = "failed", err.Error()
				log.Printf("cluster: %s share %s on %s: %v", verb, sh.Name, n.Name, err)
			} else {
				res.State = "mounted"
				if w.Automount {
					res.State = "waiting"
				}
				log.Printf("cluster: %s share %s on %s: done", verb, sh.Name, n.Name)
			}
		}
		if err == nil && (verb != "" || !seen) {
			r.mu.Lock()
			r.applied[key] = sh.PassGen
			r.mu.Unlock()
		}
		if res.State == "failed" && res.Error == "" {
			res.Error = "systemd couldn't mount it; see the node's Network shares"
		}
		out = append(out, res)
	}
	// Cluster shares that aren't meant for this node (anymore) go.
	for _, m := range list.Mounts {
		if !m.Cluster || slices.ContainsFunc(mine, func(sh Share) bool { return sh.Name == m.Name }) {
			continue
		}
		err := r.nodes.JSON("", n.ID, "POST", "/storage/mounts/"+url.PathEscape(m.Name)+"/remove", map[string]bool{"cluster": true}, nil, time.Minute)
		if err != nil {
			log.Printf("cluster: removing share %s from %s: %v", m.Name, n.Name, err)
		} else {
			log.Printf("cluster: share %s removed from %s", m.Name, n.Name)
		}
	}
	return out
}
