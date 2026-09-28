// Package nodes is the main node's list of paired nodes (/var/lib/dashboard/nodes.json)
// and the client side of pairing and proxying. Pairing trusts the node's certificate on
// first use; from then on its fingerprint is pinned and every request carries the node's
// API token.
package nodes

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/tlsutil"
)

// UserHeader tells a node who is acting, so its terminal runs as that person.
const UserHeader = "X-Dashboard-User"

type Node struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Address     string `json:"address"` // host:port
	Token       string `json:"token"`
	Fingerprint string `json:"fingerprint"`
	AddedAt     int64  `json:"addedAt"`
}

// Public is a Node without its token, for the browser.
type Public struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Address     string `json:"address"`
	Fingerprint string `json:"fingerprint"`
	AddedAt     int64  `json:"addedAt"`
}

func (n Node) Public() Public {
	return Public{ID: n.ID, Name: n.Name, Address: n.Address, Fingerprint: n.Fingerprint, AddedAt: n.AddedAt}
}

type Store struct {
	path    string
	mu      sync.Mutex
	nodes   []Node
	clients map[string]*http.Client
}

func Open() (*Store, error) {
	s := &Store{path: filepath.Join(config.StateDir, "nodes.json"), clients: map[string]*http.Client{}}
	b, err := os.ReadFile(s.path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	return s, json.Unmarshal(b, &s.nodes)
}

func (s *Store) saveLocked() error {
	b, err := json.MarshalIndent(s.nodes, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0o700); err != nil {
		return err
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, s.path)
}

func (s *Store) List() []Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Node(nil), s.nodes...)
}

func (s *Store) Get(id string) (Node, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, n := range s.nodes {
		if n.ID == id {
			return n, true
		}
	}
	return Node{}, false
}

func (s *Store) Remove(id string) (Node, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i, n := range s.nodes {
		if n.ID == id {
			s.nodes = append(s.nodes[:i], s.nodes[i+1:]...)
			delete(s.clients, id)
			return n, s.saveLocked()
		}
	}
	return Node{}, errors.New("no such node")
}

func (s *Store) Rename(id, name string) error {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 64 {
		return errors.New("name must be 1-64 characters")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.nodes {
		if s.nodes[i].ID == id {
			s.nodes[i].Name = name
			return s.saveLocked()
		}
	}
	return errors.New("no such node")
}

// NormalizeAddress accepts "host", "host:port" or "https://host:port/".
func NormalizeAddress(a string) (string, error) {
	a = strings.TrimSpace(a)
	a = strings.TrimPrefix(strings.TrimPrefix(a, "https://"), "http://")
	a = strings.TrimSuffix(a, "/")
	if a == "" || strings.ContainsAny(a, "/?#@ ") {
		return "", errors.New("enter the node's IP address or hostname, optionally with :port")
	}
	host, port, err := net.SplitHostPort(a)
	if err != nil {
		host, port = strings.Trim(a, "[]"), strconv.Itoa(config.DefaultPort)
	}
	if p, err := strconv.Atoi(port); err != nil || !config.ValidPort(p) {
		return "", errors.New("invalid port")
	}
	return net.JoinHostPort(host, port), nil
}

func newClient(fingerprint string, seen *string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			TLSClientConfig:     tlsutil.PinnedConfig(fingerprint, seen),
			TLSHandshakeTimeout: 10 * time.Second,
			IdleConnTimeout:     90 * time.Second,
		},
	}
}

// client returns the node's pinned client; no overall timeout, since it also carries
// websockets and long compose runs. Callers set deadlines through the request context.
func (s *Store) client(n Node) *http.Client {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.clients[n.ID]
	if c == nil {
		c = newClient(n.Fingerprint, nil, 0)
		s.clients[n.ID] = c
	}
	return c
}

type pairResponse struct {
	Token       string `json:"token"`
	Hostname    string `json:"hostname"`
	Fingerprint string `json:"fingerprint"`
	Error       string `json:"error"`
}

// Pair exchanges the node's one-time pairing password for its API token and stores it.
func (s *Store) Pair(address, password, name string) (Node, error) {
	addr, err := NormalizeAddress(address)
	if err != nil {
		return Node{}, err
	}
	var seen string
	c := newClient("", &seen, 20*time.Second)
	body, _ := json.Marshal(map[string]string{"password": config.NormalizePairPassword(password)})
	resp, err := c.Post("https://"+addr+"/pair", "application/json", bytes.NewReader(body))
	if err != nil {
		return Node{}, fmt.Errorf("couldn't reach %s: %v (is `dashboard enable node` done there?)", addr, err)
	}
	defer resp.Body.Close()
	var pr pairResponse
	json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&pr)
	if resp.StatusCode != http.StatusOK {
		if pr.Error == "" {
			pr.Error = resp.Status
		}
		return Node{}, errors.New(pr.Error)
	}
	if pr.Token == "" || pr.Fingerprint != seen {
		return Node{}, errors.New("the node's answer didn't match its certificate")
	}
	if name = strings.TrimSpace(name); name == "" {
		name = pr.Hostname
	}
	id := make([]byte, 6)
	rand.Read(id)
	n := Node{ID: hex.EncodeToString(id), Name: name, Address: addr, Token: pr.Token,
		Fingerprint: seen, AddedAt: time.Now().Unix()}

	s.mu.Lock()
	defer s.mu.Unlock()
	// Pairing the same address again replaces the old entry (the node forgot the old token).
	for i := range s.nodes {
		if s.nodes[i].Address == addr {
			s.nodes = append(s.nodes[:i], s.nodes[i+1:]...)
			break
		}
	}
	s.nodes = append(s.nodes, n)
	return n, s.saveLocked()
}

// Unpair tells the node to forget its token. Failures are ignored: the node may be gone.
func (s *Store) Unpair(n Node) {
	req, _ := http.NewRequest("POST", "https://"+n.Address+"/unpair", nil)
	req.Header.Set("Authorization", "Bearer "+n.Token)
	c := newClient(n.Fingerprint, nil, 10*time.Second)
	if resp, err := c.Do(req); err == nil {
		resp.Body.Close()
	}
}

// Get fetches a JSON endpoint from a node's API into out.
func (s *Store) GetJSON(n Node, path string, timeout time.Duration, out any) error {
	req, _ := http.NewRequest("GET", "https://"+n.Address+"/api/v1"+path, nil)
	req.Header.Set("Authorization", "Bearer "+n.Token)
	c := newClient(n.Fingerprint, nil, timeout)
	c.Transport = s.client(n).Transport
	resp, err := c.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		var e struct{ Error string }
		json.NewDecoder(resp.Body).Decode(&e)
		if e.Error == "" {
			e.Error = resp.Status
		}
		return errors.New(e.Error)
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

// Proxy forwards a browser request (path already stripped to the node API path) to
// the node. The browser's cookie and CSRF header stay on the main node; the node gets
// the bearer token and the acting user instead. Websocket upgrades pass through.
func (s *Store) Proxy(n Node, user string) http.Handler {
	target := &url.URL{Scheme: "https", Host: n.Address}
	return &httputil.ReverseProxy{
		Transport: s.client(n).Transport,
		Rewrite: func(pr *httputil.ProxyRequest) {
			pr.SetURL(target)
			pr.Out.URL.Path = "/api/v1" + pr.In.URL.Path
			pr.Out.URL.RawPath = ""
			pr.Out.Host = n.Address
			pr.Out.Header.Del("Cookie")
			pr.Out.Header.Del("Origin")
			pr.Out.Header.Del("X-CSRF-Token")
			pr.Out.Header.Set("Authorization", "Bearer "+n.Token)
			pr.Out.Header.Set(UserHeader, user)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusBadGateway)
			json.NewEncoder(w).Encode(map[string]string{"error": fmt.Sprintf("node %s is unreachable: %v", n.Name, err)})
		},
	}
}
