package presets

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const treeJSON = `{"tree":[
 {"path":"Composes","type":"tree"},
 {"path":"Composes/Jellyfin","type":"tree"},
 {"path":"Composes/Jellyfin/compose.yml","type":"blob"},
 {"path":"Composes/Arr-Stack/compose.yml","type":"blob"},
 {"path":"Composes/Arr-Stack/note.txt","type":"blob"},
 {"path":"Composes/OnlyNote/note.txt","type":"blob"},
 {"path":"README.md","type":"blob"},
 {"path":"Composes/Deep/x/compose.yml","type":"blob"}]}`

func TestParseTree(t *testing.T) {
	ps, err := ParseTree([]byte(treeJSON))
	if err != nil || len(ps) != 2 || ps[0] != (Preset{Name: "Arr-Stack", HasNote: true}) || ps[1] != (Preset{Name: "Jellyfin"}) {
		t.Fatalf("%+v %v", ps, err)
	}
}

func TestSource(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/repos/X27/Composes/git/trees/main":
			w.Write([]byte(treeJSON))
		case "/X27/Composes/raw/branch/main/Composes/Arr-Stack/compose.yml":
			w.Write([]byte("services: {}\n"))
		case "/X27/Composes/raw/branch/main/Composes/Arr-Stack/note.txt":
			w.Write([]byte("chown it"))
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	s, err := New(srv.URL + "/X27/Composes")
	if err != nil {
		t.Fatal(err)
	}
	if ps, err := s.List(); err != nil || len(ps) != 2 {
		t.Fatalf("%v %v", ps, err)
	}
	f, err := s.Get("Arr-Stack")
	if err != nil || f.Compose != "services: {}\n" || f.Note != "chown it" {
		t.Fatalf("%+v %v", f, err)
	}
	if _, err := s.Get("Missing"); err == nil {
		t.Fatal("missing preset")
	}
	for _, bad := range []string{"..", "a/b", "x?y"} {
		if _, err := s.Get(bad); err == nil || !strings.Contains(err.Error(), "bad preset") {
			t.Errorf("%q: %v", bad, err)
		}
	}
	for _, bad := range []string{"ftp://x/a/b", "https://codeberg.org/onlyowner", "https://codeberg.org/a/b/c"} {
		if _, err := New(bad); err == nil {
			t.Errorf("accepted %q", bad)
		}
	}
}
