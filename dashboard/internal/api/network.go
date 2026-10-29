package api

import (
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/network"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/nodes"
)

// networkRoutes: the node's IPv4 addresses and DNS, changed under an auto-revert.
func (l *Local) networkRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /settings/network", func(w http.ResponseWriter, r *http.Request) {
		conns, err := network.List()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"connections": conns, "pending": network.PendingChange(),
			"revertAfter": int(network.RevertAfter.Seconds())})
	})
	mux.HandleFunc("POST /settings/network/keep", func(w http.ResponseWriter, r *http.Request) {
		result(w, r, "network keep", "", network.Confirm(true))
	})
	mux.HandleFunc("POST /settings/network/revert", func(w http.ResponseWriter, r *http.Request) {
		result(w, r, "network revert", "", network.Confirm(false))
	})
	mux.HandleFunc("POST /settings/network/{uuid}", func(w http.ResponseWriter, r *http.Request) {
		var c network.Conn
		if err := readJSON(r, &c); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		c.UUID = r.PathValue("uuid")
		p, err := network.Apply(c)
		if err != nil {
			result(w, r, "network change "+c.UUID, "", err)
			return
		}
		log.Printf("%s: network change %s: %s %v gateway %q dns %v", userOf(r), p.Name, c.Method, c.Addresses, c.Gateway, c.DNS)
		writeJSON(w, http.StatusOK, map[string]any{"pending": p})
	})
}

// hasIP reports whether ip is one of the addresses (with or without a /prefix).
func hasIP(addrs []string, ip string) bool {
	for _, a := range addrs {
		if h, _, _ := strings.Cut(a, "/"); h == ip {
			return true
		}
	}
	return false
}

// followNode moves the main node's record of n to address and keeps the node's pending
// network change once it answers there. If it doesn't before the node would revert, the
// old address goes back.
func followNode(store *nodes.Store, n nodes.Node, address string) {
	if err := store.SetAddress(n.ID, address); err != nil {
		log.Printf("network change on node %s: %v", n.Name, err)
		return
	}
	deadline := time.Now().Add(network.RevertAfter - 20*time.Second)
	for time.Now().Before(deadline) {
		time.Sleep(3 * time.Second)
		moved, ok := store.Get(n.ID)
		if !ok {
			return
		}
		if err := store.PostJSON(moved, "/settings/network/keep", 5*time.Second, nil); err == nil {
			log.Printf("node %s moved to %s, network change kept", n.Name, address)
			return
		}
	}
	log.Printf("node %s didn't answer on %s; it reverts its network change, back to %s", n.Name, address, n.Address)
	store.SetAddress(n.ID, n.Address)
}
