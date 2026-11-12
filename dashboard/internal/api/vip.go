package api

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/cluster"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/nodes"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/vip"
)

// vipRoutes are every node's half of the virtual IP: its status, its settings (the main node
// sets every member's), and a member receiving the main node's state.
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
		c.MainName, c.MainAddr, c.SyncedAt = old.MainName, old.MainAddr, old.SyncedAt
		what := "turn off the virtual IP"
		if c.Enabled {
			what = fmt.Sprintf("set the virtual IP %s/%d (priority %d)", c.Address, c.Prefix, c.Priority)
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
		if len(b.Cluster) > 0 {
			if err := cluster.Receive(b.Cluster); err != nil {
				writeErr(w, http.StatusBadRequest, err)
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
	})
}

// memberSync is the last copy sent to one member.
type memberSync struct {
	At  int64  `json:"at"`
	Err string `json:"error,omitempty"`
}

// vipSync keeps every member's copy of the main node's state fresh.
type vipSync struct {
	store *nodes.Store
	mu    sync.Mutex
	last  map[string]memberSync
	kick  chan struct{}
}

func newVIPSync(store *nodes.Store) *vipSync {
	s := &vipSync{store: store, last: map[string]memberSync{}, kick: make(chan struct{}, 1)}
	go s.loop()
	s.Kick()
	return s
}

// mainAddr is where the members reach the main node: its own address on the virtual IP's
// subnet, at its port.
func mainAddr(c cluster.VIPConfig) string {
	cfg, _ := config.Load()
	iface, err := vip.FindInterface(c.Address, c.Prefix)
	if err != nil {
		return ""
	}
	return net.JoinHostPort(iface.IP, strconv.Itoa(cfg.Port))
}

// push sends the bundle to every member but the main node, and returns one error per member.
func (s *vipSync) push() map[string]error {
	st := cluster.Load()
	errs := map[string]error{}
	if !st.VIP.Enabled {
		return errs
	}
	host, _ := os.Hostname()
	raw, _ := json.Marshal(st.ForMembers())
	addr := mainAddr(st.VIP)
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, id := range st.MemberIDs() {
		if id == cluster.LocalID {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			var err error
			n, ok := s.store.Get(id)
			if !ok {
				err = fmt.Errorf("node %s isn't paired anymore", id)
			} else {
				var b vip.Bundle
				if b, err = vip.MakeBundle(s.store, n.ID, host); err == nil {
					b.Cluster, b.MainAddr = raw, addr
					err = s.store.PostBody(n, "", "/vip/sync", 30*time.Second, b, nil)
				}
			}
			mu.Lock()
			errs[id] = err
			mu.Unlock()
		}()
	}
	wg.Wait()
	s.mu.Lock()
	for id, err := range errs {
		ms := memberSync{At: s.last[id].At}
		if err != nil {
			ms.Err = err.Error()
		} else {
			ms.At = time.Now().Unix()
		}
		s.last[id] = ms
	}
	s.mu.Unlock()
	return errs
}

func (s *vipSync) status() map[string]memberSync {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := map[string]memberSync{}
	for k, v := range s.last {
		out[k] = v
	}
	return out
}

func (s *vipSync) loop() {
	t := time.NewTicker(time.Minute)
	for {
		select {
		case <-t.C:
		case <-s.kick:
		}
		for id, err := range s.push() {
			if err != nil {
				log.Printf("virtual IP: copying to %s: %v", id, err)
			}
		}
	}
}

// Kick copies to the members soon (after nodes, tokens or the cluster settings change).
func (s *vipSync) Kick() {
	select {
	case s.kick <- struct{}{}:
	default:
	}
}
