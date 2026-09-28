package api

import (
	"bytes"
	"crypto/tls"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/config"
	"github.com/gamerx27/x27-linux-homelab/dashboard/internal/tlsutil"
)

// One process plays both sides: the node's and the main's config/state live in the same
// temp dirs, which works because the node only uses the config file and the main only
// nodes.json.
func TestPairAndProxy(t *testing.T) {
	dir := t.TempDir()
	config.Path = filepath.Join(dir, "dashboard.conf")
	config.StateDir = dir
	cert, err := tlsutil.Ensure(filepath.Join(dir, "tls"))
	if err != nil {
		t.Fatal(err)
	}

	pw := config.NewPairPassword()
	if err := (config.Config{Mode: config.ModeNode, Port: 9090, PairHash: config.Hash(pw)}).Save(); err != nil {
		t.Fatal(err)
	}
	node := httptest.NewUnstartedServer(NewNode(Options{Version: "test", Cert: cert}))
	node.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	node.StartTLS()
	defer node.Close()
	nodeAddr := strings.TrimPrefix(node.URL, "https://")

	mainH, err := NewMain(Options{Version: "test", Cert: cert, Web: fstest.MapFS{"index.html": {Data: []byte("ui")}},
		Auth: func(u, p string) error {
			if u == "root" && p == "pw" {
				return nil
			}
			return errors.New("no")
		}})
	if err != nil {
		t.Fatal(err)
	}
	main := httptest.NewServer(mainH)
	defer main.Close()
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar}

	call := func(method, path, csrf string, body any) (int, map[string]any) {
		var rd io.Reader
		if body != nil {
			b, _ := json.Marshal(body)
			rd = bytes.NewReader(b)
		}
		req, _ := http.NewRequest(method, main.URL+path, rd)
		if csrf != "" {
			req.Header.Set("X-CSRF-Token", csrf)
		}
		resp, err := c.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		raw, _ := io.ReadAll(resp.Body)
		json.Unmarshal(raw, &out)
		if out == nil {
			out = map[string]any{"raw": string(raw)}
		}
		return resp.StatusCode, out
	}

	if code, _ := call("GET", "/api/nodes", "", nil); code != 401 {
		t.Fatalf("unauthenticated api: %d", code)
	}
	if code, _ := call("POST", "/auth/login", "", map[string]string{"username": "root", "password": "bad"}); code != 401 {
		t.Fatalf("bad login: %d", code)
	}
	code, login := call("POST", "/auth/login", "", map[string]string{"username": "root", "password": "pw"})
	if code != 200 {
		t.Fatalf("login: %d %v", code, login)
	}
	csrf := login["csrf"].(string)

	if code, _ := call("POST", "/api/nodes", "", map[string]string{"address": nodeAddr, "password": pw}); code != 403 {
		t.Fatalf("missing CSRF accepted: %d", code)
	}
	if code, r := call("POST", "/api/nodes", csrf, map[string]string{"address": nodeAddr, "password": "WRONG-WRONG"}); code != 400 {
		t.Fatalf("wrong password: %d %v", code, r)
	}
	code, n := call("POST", "/api/nodes", csrf, map[string]string{"address": nodeAddr, "password": strings.ToLower(pw), "name": "n1"})
	if code != 200 || n["fingerprint"] != tlsutil.Fingerprint(cert) {
		t.Fatalf("pair: %d %v", code, n)
	}
	if _, has := n["token"]; has {
		t.Fatal("token leaked to the browser")
	}
	id := n["id"].(string)

	// The password is single use.
	if code, _ := call("POST", "/api/nodes", csrf, map[string]string{"address": nodeAddr, "password": pw}); code != 400 {
		t.Fatalf("reused password accepted: %d", code)
	}

	// Proxied call reaches the node's API with the token.
	if code, r := call("GET", "/api/n/"+id+"/settings", "", nil); code != 200 || r["hostname"] == nil {
		t.Fatalf("proxy: %d %v", code, r)
	}
	// The node refuses callers without the token.
	resp, err := (&http.Client{Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}}).Get(node.URL + "/api/v1/settings")
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("node without token: %v %v", resp.StatusCode, err)
	}

	// Removing the node unpairs it; the proxy then 404s.
	if code, _ := call("DELETE", "/api/nodes/"+id, csrf, nil); code != 200 {
		t.Fatalf("remove: %d", code)
	}
	if code, _ := call("GET", "/api/n/"+id+"/settings", "", nil); code != 404 {
		t.Fatalf("removed node still proxied: %d", code)
	}
}
