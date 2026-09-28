// Package auth logs people into the main node's UI with their Linux password (PAM
// service "dashboard") and keeps their sessions in memory; a restart logs everyone out.
// Only members of the wheel group get in, since the UI hands out a terminal and root-level
// controls.
package auth

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"os/user"
	"slices"
	"sync"
	"time"
)

const (
	CookieName  = "dashboard_session"
	CSRFHeader  = "X-CSRF-Token"
	sessionIdle = 12 * time.Hour

	maxFailures = 5
	lockout     = time.Minute
)

var ErrLocked = errors.New("too many failed logins, try again in a minute")
var ErrDenied = errors.New("wrong username or password")
var ErrNotAdmin = errors.New("only members of the wheel group can use the dashboard")

type Session struct {
	User    string
	CSRF    string
	expires time.Time
}

// Authenticator checks a username + password. PAM in production, a stub in --dev.
type Authenticator func(username, password string) error

type Manager struct {
	auth     Authenticator
	anyGroup bool // --dev: no wheel check
	mu       sync.Mutex
	sessions map[string]*Session
	failures map[string]*failure
}

type failure struct {
	count int
	until time.Time
}

// NewManager with requireWheel false is only for --dev.
func NewManager(a Authenticator, requireWheel bool) *Manager {
	return &Manager{auth: a, anyGroup: !requireWheel, sessions: map[string]*Session{}, failures: map[string]*failure{}}
}

func random() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// Login checks the password and group, and on success sets the session cookie.
func (m *Manager) Login(w http.ResponseWriter, r *http.Request, username, password string) (*Session, error) {
	ip := clientIP(r)
	m.mu.Lock()
	if f := m.failures[ip]; f != nil && time.Now().Before(f.until) {
		m.mu.Unlock()
		return nil, ErrLocked
	}
	m.mu.Unlock()

	err := m.auth(username, password)
	if err == nil && !m.anyGroup && !InWheel(username) {
		err = ErrNotAdmin
	}
	if err != nil {
		m.mu.Lock()
		f := m.failures[ip]
		if f == nil {
			f = &failure{}
			m.failures[ip] = f
		}
		f.count++
		if f.count >= maxFailures {
			f.count = 0
			f.until = time.Now().Add(lockout)
		}
		m.mu.Unlock()
		if errors.Is(err, ErrNotAdmin) {
			return nil, err
		}
		return nil, ErrDenied
	}

	s := &Session{User: username, CSRF: random(), expires: time.Now().Add(sessionIdle)}
	id := random()
	m.mu.Lock()
	delete(m.failures, ip)
	m.sessions[id] = s
	m.mu.Unlock()
	http.SetCookie(w, &http.Cookie{
		Name: CookieName, Value: id, Path: "/",
		HttpOnly: true, Secure: r.TLS != nil, SameSite: http.SameSiteStrictMode,
	})
	return s, nil
}

func (m *Manager) Logout(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(CookieName); err == nil {
		m.mu.Lock()
		delete(m.sessions, c.Value)
		m.mu.Unlock()
	}
	http.SetCookie(w, &http.Cookie{Name: CookieName, Value: "", Path: "/", MaxAge: -1})
}

// Session returns the live session for the request, sliding its expiry.
func (m *Manager) Session(r *http.Request) *Session {
	c, err := r.Cookie(CookieName)
	if err != nil {
		return nil
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.sessions[c.Value]
	if s == nil {
		return nil
	}
	if time.Now().After(s.expires) {
		delete(m.sessions, c.Value)
		return nil
	}
	s.expires = time.Now().Add(sessionIdle)
	return s
}

// CheckCSRF: every request that changes something carries the session's token in a header,
// which a page on another origin can't read or set.
func CheckCSRF(s *Session, r *http.Request) bool {
	switch r.Method {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return subtle.ConstantTimeCompare([]byte(r.Header.Get(CSRFHeader)), []byte(s.CSRF)) == 1
}

// InWheel reports whether the user is root or in the wheel group.
func InWheel(username string) bool {
	if username == "root" {
		return true
	}
	u, err := user.Lookup(username)
	if err != nil {
		return false
	}
	g, err := user.LookupGroup("wheel")
	if err != nil {
		return false
	}
	if u.Gid == g.Gid {
		return true
	}
	ids, err := u.GroupIds()
	return err == nil && slices.Contains(ids, g.Gid)
}
