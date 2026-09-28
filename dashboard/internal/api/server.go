package api

import (
	"crypto/tls"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/auth"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/nodes"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/tlsutil"
)

type Options struct {
	Version string
	Mode    string
	Dev     bool
	Cert    tls.Certificate
	Web     fs.FS
	Auth    auth.Authenticator
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", "default-src 'self'; style-src 'self' 'unsafe-inline'; img-src 'self' data:; frame-ancestors 'none'")
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "no-referrer")
		if r.TLS != nil {
			h.Set("Strict-Transport-Security", "max-age=31536000")
		}
		next.ServeHTTP(w, r)
	})
}

// NewMain serves the UI, login, the node list, and every node's API under /api/n/{id}/.
func NewMain(o Options) (http.Handler, error) {
	store, err := nodes.Open()
	if err != nil {
		return nil, err
	}
	// Same-origin websockets only (gorilla's default origin check).
	local := NewLocal(o.Version, o.Dev, &websocket.Upgrader{})
	localAPI := local.Handler()
	sessions := auth.NewManager(o.Auth, !o.Dev)
	fingerprint := tlsutil.Fingerprint(o.Cert)

	mux := http.NewServeMux()
	static := http.FileServerFS(o.Web)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Embedded files have no modification time, so make browsers revalidate: the UI
		// changes with every image update.
		w.Header().Set("Cache-Control", "no-cache")
		static.ServeHTTP(w, r)
	}))

	mux.HandleFunc("POST /auth/login", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Username, Password string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		s, err := sessions.Login(w, r, body.Username, body.Password)
		if err != nil {
			log.Printf("failed login for %q from %s", body.Username, r.RemoteAddr)
			writeErr(w, http.StatusUnauthorized, err)
			return
		}
		log.Printf("%s logged in from %s", s.User, r.RemoteAddr)
		writeJSON(w, http.StatusOK, map[string]string{"user": s.User, "csrf": s.CSRF})
	})
	mux.HandleFunc("POST /auth/logout", func(w http.ResponseWriter, r *http.Request) {
		sessions.Logout(w, r)
		writeJSON(w, http.StatusOK, map[string]string{})
	})
	mux.HandleFunc("GET /auth/me", func(w http.ResponseWriter, r *http.Request) {
		s := sessions.Session(r)
		if s == nil {
			writeErr(w, http.StatusUnauthorized, errors.New("not logged in"))
			return
		}
		host, _ := os.Hostname()
		writeJSON(w, http.StatusOK, map[string]string{"user": s.User, "csrf": s.CSRF,
			"version": o.Version, "hostname": host, "fingerprint": fingerprint})
	})

	api := http.NewServeMux()
	api.HandleFunc("GET /api/nodes", func(w http.ResponseWriter, r *http.Request) {
		type entry struct {
			nodes.Public
			Local   bool     `json:"local"`
			Online  bool     `json:"online"`
			Error   string   `json:"error,omitempty"`
			Summary *Summary `json:"summary,omitempty"`
		}
		host, _ := os.Hostname()
		ls := local.summary()
		out := []entry{{Public: nodes.Public{ID: "local", Name: host, Address: "this node", Fingerprint: fingerprint},
			Local: true, Online: true, Summary: &ls}}
		list := store.List()
		rest := make([]entry, len(list))
		var wg sync.WaitGroup
		for i, n := range list {
			wg.Add(1)
			go func() {
				defer wg.Done()
				e := entry{Public: n.Public()}
				var s Summary
				if err := store.GetJSON(n, "/summary", 6*time.Second, &s); err != nil {
					e.Error = err.Error()
				} else {
					e.Online, e.Summary = true, &s
				}
				rest[i] = e
			}()
		}
		wg.Wait()
		writeJSON(w, http.StatusOK, append(out, rest...))
	})
	api.HandleFunc("POST /api/nodes", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name, Address, Password string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		n, err := store.Pair(body.Address, body.Password, body.Name)
		if err != nil {
			log.Printf("%s: pairing %s failed: %v", userOf(r), body.Address, err)
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		log.Printf("%s: paired node %s (%s)", userOf(r), n.Name, n.Address)
		writeJSON(w, http.StatusOK, n.Public())
	})
	api.HandleFunc("POST /api/nodes/{id}/rename", func(w http.ResponseWriter, r *http.Request) {
		var body struct{ Name string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		result(w, r, "rename node "+r.PathValue("id"), "", store.Rename(r.PathValue("id"), body.Name))
	})
	api.HandleFunc("DELETE /api/nodes/{id}", func(w http.ResponseWriter, r *http.Request) {
		n, err := store.Remove(r.PathValue("id"))
		if err == nil {
			go store.Unpair(n)
		}
		result(w, r, "remove node "+n.Name, "", err)
	})
	api.HandleFunc("/api/n/{id}/", func(w http.ResponseWriter, r *http.Request) {
		id := r.PathValue("id")
		prefix := "/api/n/" + id
		if id == "local" {
			http.StripPrefix(prefix, localAPI).ServeHTTP(w, r)
			return
		}
		n, ok := store.Get(id)
		if !ok {
			writeErr(w, http.StatusNotFound, errors.New("no such node"))
			return
		}
		http.StripPrefix(prefix, store.Proxy(n, userOf(r))).ServeHTTP(w, r)
	})

	mux.Handle("/api/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s := sessions.Session(r)
		if s == nil {
			writeErr(w, http.StatusUnauthorized, errors.New("not logged in"))
			return
		}
		if !auth.CheckCSRF(s, r) {
			writeErr(w, http.StatusForbidden, errors.New("missing or wrong CSRF token, reload the page"))
			return
		}
		api.ServeHTTP(w, withUser(r, s.User))
	}))
	return securityHeaders(mux), nil
}

// NewNode serves only the API, to the one main node holding its token, plus /pair.
func NewNode(o Options) http.Handler {
	// The main node proxies browser websockets; the bearer token is the protection here.
	local := NewLocal(o.Version, o.Dev, &websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }})
	localAPI := local.Handler()
	fingerprint := tlsutil.Fingerprint(o.Cert)

	var pairMu sync.Mutex
	var pairFails int
	var pairLockUntil time.Time

	mux := http.NewServeMux()
	mux.HandleFunc("POST /pair", func(w http.ResponseWriter, r *http.Request) {
		pairMu.Lock()
		defer pairMu.Unlock()
		if time.Now().Before(pairLockUntil) {
			writeErr(w, http.StatusTooManyRequests, errors.New("too many wrong pairing passwords, wait a minute"))
			return
		}
		var body struct{ Password string }
		if err := readJSON(r, &body); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return
		}
		cfg, err := config.Load()
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		if cfg.PairHash == "" {
			writeErr(w, http.StatusForbidden, errors.New("this node isn't waiting to be paired (run `dashboard pair` on it)"))
			return
		}
		if !config.Check(cfg.PairHash, config.NormalizePairPassword(body.Password)) {
			if pairFails++; pairFails >= 5 {
				pairFails, pairLockUntil = 0, time.Now().Add(time.Minute)
			}
			log.Printf("wrong pairing password from %s", r.RemoteAddr)
			writeErr(w, http.StatusForbidden, errors.New("wrong pairing password"))
			return
		}
		token := config.NewToken()
		cfg.TokenHash, cfg.PairHash = config.Hash(token), ""
		if err := cfg.Save(); err != nil {
			writeErr(w, http.StatusInternalServerError, err)
			return
		}
		pairFails = 0
		host, _ := os.Hostname()
		log.Printf("paired with main node at %s", r.RemoteAddr)
		writeJSON(w, http.StatusOK, map[string]string{"token": token, "hostname": host, "fingerprint": fingerprint})
	})

	authorized := func(r *http.Request) bool {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok || tok == "" {
			return false
		}
		cfg, err := config.Load()
		return err == nil && config.Check(cfg.TokenHash, tok)
	}

	mux.HandleFunc("POST /unpair", func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			writeErr(w, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}
		cfg, err := config.Load()
		if err == nil {
			cfg.TokenHash = ""
			err = cfg.Save()
		}
		log.Printf("main node at %s unpaired this node", r.RemoteAddr)
		result(w, r, "unpair", "", err)
	})

	mux.Handle("/api/v1/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !authorized(r) {
			writeErr(w, http.StatusUnauthorized, errors.New("unauthorized"))
			return
		}
		http.StripPrefix("/api/v1", localAPI).ServeHTTP(w, withUser(r, r.Header.Get(nodes.UserHeader)))
	}))
	return securityHeaders(mux)
}
