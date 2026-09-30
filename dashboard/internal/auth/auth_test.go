package auth

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A session outlives a restart; the file never holds the cookie itself.
func TestSessionsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	ok := func(u, p string) error { return nil }
	m := NewManager(ok, false)
	m.Persist(path)
	w := httptest.NewRecorder()
	s, err := m.Login(w, httptest.NewRequest("POST", "/auth/login", nil), "root", "pw")
	if err != nil {
		t.Fatal(err)
	}
	cookie := w.Result().Cookies()[0]
	b, _ := os.ReadFile(path)
	if strings.Contains(string(b), cookie.Value) {
		t.Fatal("the session file holds the cookie")
	}

	m2 := NewManager(ok, false)
	m2.Persist(path)
	r := httptest.NewRequest("GET", "/auth/me", nil)
	r.AddCookie(cookie)
	got := m2.Session(r)
	if got == nil || got.User != "root" || got.CSRF != s.CSRF {
		t.Fatalf("after restart: %+v", got)
	}

	m2.Logout(httptest.NewRecorder(), r)
	m3 := NewManager(ok, false)
	m3.Persist(path)
	if m3.Session(r) != nil {
		t.Fatal("logged-out session came back")
	}
}
