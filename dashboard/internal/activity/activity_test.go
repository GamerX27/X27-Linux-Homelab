package activity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAddReadRotate(t *testing.T) {
	p := filepath.Join(t.TempDir(), "activity.jsonl")
	Init(p)
	defer Init("")
	Add(Entry{Time: 1, User: "anna", Via: "web", Action: "stack start web", OK: true})
	Add(Entry{Time: 2, User: "bob", Via: "token:claude", Action: "container restart db", Error: "no such container"})
	Add(Entry{Time: 3, User: "anna", Via: "web", Action: "files delete ~/x", OK: true})

	es, _ := Read(Query{})
	if len(es) != 3 || es[0].Time != 3 {
		t.Fatalf("newest first: %+v", es)
	}
	if es, _ := Read(Query{User: "anna"}); len(es) != 2 {
		t.Fatal("user filter")
	}
	if es, _ := Read(Query{Failed: true}); len(es) != 1 || es[0].User != "bob" {
		t.Fatal("failed filter")
	}
	if es, _ := Read(Query{Text: "CLAUDE"}); len(es) != 1 {
		t.Fatal("text matches via")
	}
	if es, _ := Read(Query{Before: 3, Limit: 1}); len(es) != 1 || es[0].Time != 2 {
		t.Fatalf("paging %+v", es)
	}

	// Past MaxSize the file moves to .1, and both are still read.
	os.WriteFile(p, []byte(strings.Repeat(" ", MaxSize)+"\n"), 0o600)
	Add(Entry{Time: 4, User: "anna", Action: "after rotate", OK: true})
	if _, err := os.Stat(p + ".1"); err != nil {
		t.Fatal("not rotated")
	}
	if es, _ := Read(Query{}); len(es) != 1 || es[0].Time != 4 {
		t.Fatalf("after rotate %+v", es)
	}
}
